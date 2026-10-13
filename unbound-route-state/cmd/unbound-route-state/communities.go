package main

import (
	"bufio"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"unbound-route-state/internal/state"
)

// communityRule is one [pattern1, pattern2, ...] block from
// communities.conf (v2 format - see communities.conf.example
// for the full syntax). Everything except patterns/communities/
// largeCommunities is optional and left at its zero value when absent.
type communityRule struct {
	// patterns is the EFFECTIVE list used for matching and cache flushing: the
	// inline patterns of the header plus everything read from any referenced
	// domains.d list file. header is the header exactly as written (inline
	// patterns and "name.list" tokens) - for display and log messages.
	patterns         []string
	header           []string
	communities      []state.Community
	largeCommunities []state.LargeCommunity
	nextHop          string // IPv4 next-hop for A routes
	nextHopV6        string // IPv6 next-hop for AAAA routes
	asPath           []uint32
	localPref        uint32
	hasLocalPref     bool
	med              uint32
	hasMED           bool
}

// listRef is one domains.d list file referenced from a block header.
type listRef struct {
	name     string
	fp       fileFP
	exists   bool
	patterns int // valid patterns read from it
	invalid  int // lines skipped as not a hostname / *.hostname
}

// loadOpts configure list-file references in block headers.
type loadOpts struct {
	DomainsDir      string // where "name.list" tokens are looked up; "" = references are an error
	MaxFilePatterns int    // cap on patterns coming from list files in total; <= 0 = unlimited
}

type loadResult struct {
	rules []communityRule
	def   *communityRule
	refs  []listRef // in first-reference order
}

// isListRef reports whether a header token names a domains.d list file
// rather than a domain pattern. No gTLD ".list" exists, so the suffix cannot
// collide with a real domain.
func isListRef(tok string) bool { return strings.HasSuffix(strings.ToLower(tok), ".list") }

func refTag(r listRef) string {
	return fmt.Sprintf("%s|%d|%d|%t", r.name, r.fp.mtime, r.fp.size, r.exists)
}

// statRefTag is refTag computed from the file as it is NOW (change detection).
func statRefTag(dir, name string) string {
	r := listRef{name: name}
	if fi, err := os.Stat(filepath.Join(dir, name)); err == nil {
		r.fp, r.exists = fileFP{mtime: fi.ModTime().UnixNano(), size: fi.Size()}, true
	}
	return refTag(r)
}

func refsTag(refs []listRef) string {
	tags := make([]string, 0, len(refs))
	for _, r := range refs {
		tags = append(tags, refTag(r))
	}
	sort.Strings(tags)
	return strings.Join(tags, ";")
}

// validListPattern: a hostname or *.hostname (the same strictness as the
// cache-flush names).
func validListPattern(p string) bool {
	p = strings.ToLower(strings.TrimSuffix(p, "."))
	return validFlushName(strings.TrimPrefix(p, "*."))
}

// readListRef reads one list file of domains.d. The file is stat'ed BEFORE it
// is read, so an edit that lands in between makes the recorded fingerprint
// older than the content and the next watcher tick reloads again (harmless)
// instead of missing it. A missing file is not an error: lists are filled
// externally and may not have been delivered yet - it counts as empty until it
// appears (the watcher notices that too).
func readListRef(dir, name string) (listRef, []string, error) {
	ref := listRef{name: name}
	p := filepath.Join(dir, name)
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ref, nil, nil
		}
		return ref, nil, fmt.Errorf("list file %s: %w", p, err)
	}
	if !fi.Mode().IsRegular() {
		return ref, nil, fmt.Errorf("list file %s is not a regular file", p)
	}
	ref.fp, ref.exists = fileFP{mtime: fi.ModTime().UnixNano(), size: fi.Size()}, true
	b, err := os.ReadFile(p)
	if err != nil {
		return ref, nil, fmt.Errorf("list file %s: %w", p, err)
	}
	var pats []string
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if validListPattern(line) {
			pats = append(pats, line)
		} else {
			ref.invalid++
		}
	}
	ref.patterns = len(pats)
	return ref, pats, nil
}

// loadCommunityRules parses communities.conf (v2 format) without list-file
// support (a "name.list" token is an error); kept for callers/tests that
// only deal with inline patterns.
func loadCommunityRules(path string) ([]communityRule, *communityRule, error) {
	res, err := loadCommunityRulesFull(path, loadOpts{})
	return res.rules, res.def, err
}

// loadCommunityRulesFull parses communities.conf (v2 format).
//
// Format, per block:
//
//	[*.pattern1, pattern2, banks.list, ...]
//
// where each header entry is either a domain pattern or the name of a file in
// domains.d (an entry ending in ".list"); the block then covers the inline
// patterns plus every pattern in the file(s). Other keys:
//
//	community        = ASN:VALUE[, ASN:VALUE ...]
//	large_community  = ASN:DATA1:DATA2[, ...]
//	next_hop         = IPv4   (used for A routes)
//	next_hop_v6      = IPv6   (used for AAAA routes)
//	as_path          = ASN[, ASN ...]
//	local_pref       = N
//	med              = N
//
// All keys optional except the [...] header itself. A domain matching
// several blocks accumulates community + large_community from every
// match (see classify()'s doc comment for why); every other field is
// taken from whichever matching block appears FIRST in the file.
//
// The rules are kept in a plain list and matched linearly, unlike the
// reversed-label trie used by the Python module. The daemon's copy is off the
// DNS hot path and max_domain_patterns (default 5000) bounds its size, so a
// linear scan is not expected to matter. Revisit only if profiling says so.
func loadCommunityRulesFull(path string, o loadOpts) (loadResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return loadResult{}, fmt.Errorf("communities: %w", err)
	}
	defer f.Close()

	var rules []communityRule
	var defaultRule *communityRule
	var cur *communityRule

	// Each list file is read once per load, so every block that references it
	// sees the same content and the fingerprint matches what was loaded.
	type listData struct {
		ref  listRef
		pats []string
	}
	files := map[string]*listData{}
	var refOrder []string
	filePatterns := 0

	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(stripComment(sc.Text()))
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			header := line[1 : len(line)-1]

			if strings.TrimSpace(header) == "default:missed" {
				// Fallback for an fqdn that matches NO pattern rule at
				// all - see classify()'s doc comment. Not appended to
				// rules: it has no patterns, so it must never be
				// considered a normal match candidate, only the
				// explicit last-resort case classify() checks for.
				defaultRule = &communityRule{}
				cur = defaultRule
				continue
			}

			var patterns, tokens []string
			for _, p := range strings.Split(header, ",") {
				if p = strings.TrimSpace(p); p == "" {
					continue
				}
				tokens = append(tokens, p)
				if !isListRef(p) {
					patterns = append(patterns, p)
					continue
				}
				if o.DomainsDir == "" {
					return loadResult{}, fmt.Errorf("communities: line %d: %q is a list-file reference but no domains_dir is configured", lineNo, p)
				}
				if filepath.Base(p) != p || strings.HasPrefix(p, ".") || strings.ContainsAny(p, "*/\\") {
					return loadResult{}, fmt.Errorf("communities: line %d: %q is not a plain file name inside domains_dir", lineNo, p)
				}
				ld := files[p]
				if ld == nil {
					ref, pats, err := readListRef(o.DomainsDir, p)
					if err != nil {
						return loadResult{}, fmt.Errorf("communities: line %d: %w", lineNo, err)
					}
					ld = &listData{ref: ref, pats: pats}
					files[p] = ld
					refOrder = append(refOrder, p)
				}
				filePatterns += len(ld.pats)
				if o.MaxFilePatterns > 0 && filePatterns > o.MaxFilePatterns {
					return loadResult{}, fmt.Errorf("communities: line %d: patterns from list files exceed max_domain_patterns=%d", lineNo, o.MaxFilePatterns)
				}
				patterns = append(patterns, ld.pats...)
			}
			rules = append(rules, communityRule{patterns: patterns, header: tokens})
			cur = &rules[len(rules)-1]
			continue
		}

		if cur == nil {
			return loadResult{}, fmt.Errorf("communities: line %d: key=value with no preceding [patterns] header", lineNo)
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if err := cur.set(key, val); err != nil {
			return loadResult{}, fmt.Errorf("communities: line %d (%s): %w", lineNo, key, err)
		}
	}
	if err := sc.Err(); err != nil {
		return loadResult{}, fmt.Errorf("communities: %w", err)
	}

	// cur was a pointer into `rules`; every header appends a new element,
	// which can reallocate the backing array. Re-pointing on every append
	// would be fragile - instead cur is only ever used to *build* the
	// rule that's already the last element, and we never read through it
	// after the next append happens (the loop above always re-derives it
	// from len(rules)-1 right after appending). defaultRule is a
	// separate, standalone allocation (not into the rules slice), so it
	// never has this problem regardless.
	refs := make([]listRef, 0, len(refOrder))
	for _, n := range refOrder {
		refs = append(refs, files[n].ref)
	}
	return loadResult{rules: rules, def: defaultRule, refs: refs}, nil
}

// label names a rule in log messages: the header as written (so a list file
// shows as banks.list, not as thousands of expanded patterns).
func (r *communityRule) label() string {
	toks := r.header
	if len(toks) == 0 {
		toks = r.patterns
	}
	switch len(toks) {
	case 0:
		return "(no patterns)"
	case 1:
		return toks[0]
	default:
		return fmt.Sprintf("%s +%d more", toks[0], len(toks)-1)
	}
}

func (r *communityRule) set(key, val string) error {
	switch key {
	case "community":
		for _, tok := range splitCSV(val) {
			vals, err := parseColonUint32s(tok, 2)
			if err != nil {
				return fmt.Errorf("community %q: %w", tok, err)
			}
			r.communities = append(r.communities, state.NewCommunity(vals[0], vals[1]))
		}
	case "large_community":
		for _, tok := range splitCSV(val) {
			vals, err := parseColonUint32s(tok, 3)
			if err != nil {
				return fmt.Errorf("large_community %q: %w", tok, err)
			}
			r.largeCommunities = append(r.largeCommunities, state.LargeCommunity{
				ASN: vals[0], Data1: vals[1], Data2: vals[2],
			})
		}
	case "next_hop":
		if _, err := netip.ParseAddr(val); err != nil {
			return fmt.Errorf("next_hop %q: %w", val, err)
		}
		r.nextHop = val
	case "next_hop_v6":
		if _, err := netip.ParseAddr(val); err != nil {
			return fmt.Errorf("next_hop_v6 %q: %w", val, err)
		}
		r.nextHopV6 = val
	case "as_path":
		for _, tok := range splitCSV(val) {
			n, err := strconv.ParseUint(tok, 10, 32)
			if err != nil {
				return fmt.Errorf("as_path %q: %w", tok, err)
			}
			r.asPath = append(r.asPath, uint32(n))
		}
	case "local_pref":
		n, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			return err
		}
		r.localPref, r.hasLocalPref = uint32(n), true
	case "med":
		n, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			return err
		}
		r.med, r.hasMED = uint32(n), true
	}
	return nil // unknown keys ignored, same spirit as config.go
}

func splitCSV(val string) []string {
	var out []string
	for _, p := range strings.Split(val, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseColonUint32s parses "A:B" (n=2, community) or "A:B:C" (n=3,
// large_community) style tokens into a fixed 3-slot array - callers only
// read the first n slots they asked for.
func parseColonUint32s(tok string, n int) ([3]uint32, error) {
	var a [3]uint32
	parts := strings.Split(tok, ":")
	if len(parts) != n {
		return a, fmt.Errorf("expected %d colon-separated fields, got %d", n, len(parts))
	}
	for i, p := range parts {
		v, err := strconv.ParseUint(strings.TrimSpace(p), 10, 32)
		if err != nil {
			return a, err
		}
		a[i] = uint32(v)
	}
	return a, nil
}

// classify returns the merged RouteAttrs for fqdn against every matching
// rule: Communities and LargeCommunities accumulate (union) across all
// matches - a domain legitimately matching more than one group (e.g.
// both a CDN rule and a services rule) collects tags from both, mirroring
// how filters_up.conf/filters_mmdb.conf repeatedly call
// bgp_community.add(...) rather than picking just one. Every other field
// (NextHop/ASPath/LocalPref/MED) comes from whichever matching rule
// appears FIRST in the file - those are single-valued BGP attributes
// that cannot meaningfully "accumulate" the way a community list can.
//
// defaultRule (from an optional [default:missed] block) applies ONLY when NO
// rule matched at all. It covers drift between the domain lists read by the
// Python module and the patterns in this file: the module forwarded an fqdn it
// was told to track, but no rule here classifies it. Without a default such a
// route would carry no community and be invisible to every community-based
// export filter on the router. With one, the drift stays visible (the route is
// tagged with a generic "unclassified" community). A nil defaultRule leaves
// the attributes empty and the warning in transport.go fires.
func classify(fqdn string, rules []communityRule, defaultRule *communityRule) state.RouteAttrs {
	var out state.RouteAttrs
	haveFirst := false

	for _, r := range rules {
		if !matchesAnyPattern(fqdn, r.patterns) {
			continue
		}
		out.Communities = append(out.Communities, r.communities...)
		out.LargeCommunities = append(out.LargeCommunities, r.largeCommunities...)

		if !haveFirst {
			out.NextHop, out.NextHopV6 = r.nextHop, r.nextHopV6
			out.ASPath = r.asPath
			out.LocalPref, out.HasLocalPref = r.localPref, r.hasLocalPref
			out.MED, out.HasMED = r.med, r.hasMED
			haveFirst = true
		}
	}
	if !haveFirst && defaultRule != nil {
		out.Communities = append(out.Communities, defaultRule.communities...)
		out.LargeCommunities = append(out.LargeCommunities, defaultRule.largeCommunities...)
		out.NextHop, out.NextHopV6 = defaultRule.nextHop, defaultRule.nextHopV6
		out.ASPath = defaultRule.asPath
		out.LocalPref, out.HasLocalPref = defaultRule.localPref, defaultRule.hasLocalPref
		out.MED, out.HasMED = defaultRule.med, defaultRule.hasMED
	}
	return out
}

func matchesAnyPattern(fqdn string, patterns []string) bool {
	for _, p := range patterns {
		if matchesPattern(fqdn, p) {
			return true
		}
	}
	return false
}

// matchesPattern implements the suffix-wildcard rule shared with the Python
// module: "*.example.com" matches proper subdomains only, never the apex
// itself; an apex match needs its own explicit "example.com" pattern.
func matchesPattern(fqdn, pattern string) bool {
	fqdn = strings.ToLower(strings.TrimSuffix(fqdn, "."))
	pattern = strings.ToLower(strings.TrimSuffix(pattern, "."))
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		dotSuffix := "." + suffix
		return strings.HasSuffix(fqdn, dotSuffix) && len(fqdn) > len(dotSuffix)
	}
	return fqdn == pattern
}
