package main

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// ruleSet is the immutable pair {rules, defaultRule}. It is replaced as a
// whole through an atomic.Pointer, so a reader (transport.handleLine) that
// does one Load() can never see rules from one generation and defaultRule
// from another. Never mutate a ruleSet after it has been Store()d.
type ruleSet struct {
	rules       []communityRule
	defaultRule *communityRule

	// Observability: events that matched each rule of THIS generation
	// (hits[i] belongs to rules[i]); reset by every reload because a new
	// ruleSet is built. Always handled through *ruleSet, never copied.
	hits        []atomic.Uint64
	defaultHits atomic.Uint64

	// refs: the domains.d list files this generation was built from (empty
	// when no block references one). The watcher uses it to know which files
	// to stat; `ctl rules` shows it.
	refs []listRef
}

func newRuleSet(rules []communityRule, def *communityRule) *ruleSet {
	return &ruleSet{rules: rules, defaultRule: def, hits: make([]atomic.Uint64, len(rules))}
}

func buildRuleSet(res loadResult) *ruleSet {
	rs := newRuleSet(res.rules, res.def)
	rs.refs = res.refs
	return rs
}

// listRefByName looks up what was read from a referenced list file.
func (rs *ruleSet) listRefByName(name string) (listRef, bool) {
	if rs != nil {
		for _, r := range rs.refs {
			if r.name == name {
				return r, true
			}
		}
	}
	return listRef{}, false
}

// countHits bumps the hit counter of every rule fqdn matches (or the
// default block's, if none does) and reports whether anything counted as a
// match. One extra linear pass over the rules per event, on the transport
// reader goroutine - never on Unbound's DNS path.
func (rs *ruleSet) countHits(fqdn string) (matched bool) {
	for i := range rs.rules {
		if matchesAnyPattern(fqdn, rs.rules[i].patterns) {
			if i < len(rs.hits) {
				rs.hits[i].Add(1)
			}
			matched = true
		}
	}
	if !matched && rs.defaultRule != nil {
		rs.defaultHits.Add(1)
		return true
	}
	return matched
}

func (rs *ruleSet) empty() bool {
	return rs == nil || (len(rs.rules) == 0 && rs.defaultRule == nil)
}

// fileFP is the cheap change fingerprint of a file: mtime + size.
type fileFP struct {
	mtime int64
	size  int64
}

// rulesFP is what the watcher compares between ticks: the rules file itself
// plus every domains.d list file it references (so that editing a list also
// triggers a reload - those files are filled externally, which is the whole
// point of referencing them).
type rulesFP struct {
	main fileFP
	refs string // refsTag() form; "" when nothing is referenced
}

// currentRefsTag fingerprints the referenced list files as they are now.
func currentRefsTag(dir string, rs *ruleSet) string {
	if rs == nil || len(rs.refs) == 0 || dir == "" {
		return ""
	}
	tags := make([]string, 0, len(rs.refs))
	for _, r := range rs.refs {
		tags = append(tags, statRefTag(dir, r.name))
	}
	sort.Strings(tags)
	return strings.Join(tags, ";")
}

func statFP(path string) (fileFP, error) {
	st, err := os.Stat(path)
	if err != nil {
		return fileFP{}, err
	}
	return fileFP{mtime: st.ModTime().UnixNano(), size: st.Size()}, nil
}

// reloadRulesOnce is one watcher tick. It returns the fingerprint to
// remember: the new one after a successful swap (or when nothing changed),
// the OLD one on any failure so the next tick retries.
//
// A failed read/parse, or a reload that would replace non-empty rules with
// an empty set (typical of a truncated, half-written file), keeps the
// previous rules. Already-announced routes are NOT re-evaluated: new rules
// apply to events that arrive after the swap.
func reloadRulesOnce(path string, holder *atomic.Pointer[ruleSet], last rulesFP, o loadOpts, peerASN uint32, log logger) rulesFP {
	mainFP, err := statFP(path)
	if err != nil {
		log.Warnf("communities: reload: cannot stat rules file, keeping previous rules: %v", err)
		return last
	}
	if (rulesFP{main: mainFP, refs: currentRefsTag(o.DomainsDir, holder.Load())}) == last {
		return last
	}
	res, err := loadCommunityRulesFull(path, o)
	if err != nil {
		ctr.rulesReloadFailed.Add(1)
		log.Warnf("communities: reload failed, keeping previous rules: %v", err)
		return last
	}
	rules := res.rules
	ns := buildRuleSet(res)
	if ns.empty() && !holder.Load().empty() {
		ctr.rulesReloadFailed.Add(1)
		log.Warnf("communities: reload produced no rules (empty or truncated file?), keeping previous rules")
		return last
	}
	holder.Store(ns)
	ctr.rulesReloadOK.Add(1)
	ctr.noteRulesLoaded()
	log.Infof("communities: reloaded %d rule block(s) from %s", len(rules), path)
	warnASPathLoops(ns, peerASN, log)
	warnNextHopFamilies(ns, log)
	warnListFiles(ns, log)
	return rulesFP{main: mainFP, refs: refsTag(res.refs)}
}

// watchCommunityRules polls rules_file every interval until ctx is done.
// initial is the fingerprint taken BEFORE the startup load, so a change
// landing between that load and the first tick is not missed.
func watchCommunityRules(ctx context.Context, path string, interval time.Duration, initial rulesFP, o loadOpts, holder *atomic.Pointer[ruleSet], peerASN uint32, log logger) {
	if interval <= 0 {
		return
	}
	last := initial
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			last = reloadRulesOnce(path, holder, last, o, peerASN, log)
		}
	}
}

// warnASPathLoops emits ONE aggregated warning listing every rule whose
// as_path contains peerASN (the BGP neighbour's AS - BIRD's own AS). BIRD
// treats its own ASN inside a received AS_PATH as a loop and silently drops
// the route, which otherwise looks like "the route just never appears".
// One aggregated Warnf, not one per rule: the rate limiter buckets by
// format string, so per-rule messages would swallow each other.
func warnASPathLoops(rs *ruleSet, peerASN uint32, log logger) {
	if rs == nil || peerASN == 0 {
		return
	}
	var bad []string
	check := func(name string, r *communityRule) {
		for _, a := range r.asPath {
			if a == peerASN {
				bad = append(bad, name)
				return
			}
		}
	}
	for i := range rs.rules {
		check(rs.rules[i].label(), &rs.rules[i])
	}
	if rs.defaultRule != nil {
		check("default:missed", rs.defaultRule)
	}
	if len(bad) > 0 {
		log.Warnf("communities: as_path contains AS%d (the neighbour's own AS) in rule(s) [%s]: BIRD will drop such routes as an AS-path loop - remove it or use a different ASN", peerASN, strings.Join(bad, "; "))
	}
}

// warnNextHopFamilies emits ONE aggregated warning for rules whose next-hop
// keys are in the wrong address family: next_hop must be IPv4 and
// next_hop_v6 IPv6. A mismatch is not fatal - an IPv6 literal in next_hop
// is still honoured for IPv6 routes (backward compatibility), anything else
// is ignored in favour of the configured default - but it is almost always
// a config mistake worth seeing at load/reload time rather than per route.
func warnNextHopFamilies(rs *ruleSet, log logger) {
	if rs == nil {
		return
	}
	var bad []string
	check := func(name string, r *communityRule) {
		if a, err := netip.ParseAddr(r.nextHop); err == nil && !a.Is4() {
			bad = append(bad, name+": next_hop "+r.nextHop+" is IPv6 (move it to next_hop_v6)")
		}
		if a, err := netip.ParseAddr(r.nextHopV6); err == nil && !a.Is6() {
			bad = append(bad, name+": next_hop_v6 "+r.nextHopV6+" is IPv4 (it can never apply)")
		}
	}
	for i := range rs.rules {
		check(rs.rules[i].label(), &rs.rules[i])
	}
	if rs.defaultRule != nil {
		check("default:missed", rs.defaultRule)
	}
	if len(bad) > 0 {
		log.Warnf("communities: next-hop address family problem(s): %s", strings.Join(bad, "; "))
	}
}

// warnListFiles emits ONE aggregated warning about referenced list files that
// need attention: missing ones (treated as empty until they appear - lists are
// filled externally, so this is often temporary) and ones with lines that are
// not hostnames (skipped).
func warnListFiles(rs *ruleSet, log logger) {
	if rs == nil {
		return
	}
	var missing, invalid []string
	for _, r := range rs.refs {
		if !r.exists {
			missing = append(missing, r.name)
		} else if r.invalid > 0 {
			invalid = append(invalid, fmt.Sprintf("%s(%d)", r.name, r.invalid))
		}
	}
	if len(missing) == 0 && len(invalid) == 0 {
		return
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "missing in domains_dir, treated as empty until they appear: "+strings.Join(missing, ", "))
	}
	if len(invalid) > 0 {
		parts = append(parts, "lines skipped as not a hostname: "+strings.Join(invalid, ", "))
	}
	log.Warnf("communities: list file problem(s): %s", strings.Join(parts, "; "))
}
