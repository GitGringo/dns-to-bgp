package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Cache flush at startup.
//
// Why: Unbound answers repeat queries from its message cache BEFORE any
// module runs, so after this daemon (re)starts with an empty route table,
// names that are still cached never reach the Python module again until
// their TTL lapses - the BGP feed has gaps. Flushing the watched names from
// Unbound's cache makes the next client query re-resolve them, which sends
// the event and repopulates the route.
//
// How: shell out to `unbound-control` (no TLS/remote-control protocol
// reimplementation). Per name pattern:
//
//	"*.example.com"  ->  flush_zone example.com   (everything at/below it; the
//	                     apex is covered too, so a plain "example.com" next to
//	                     it needs no command of its own)
//	"example.com"    ->  flush example.com        (exact name, cheap)
//
// flush_zone walks the whole cache and Unbound documents it as slow, so
// the targets are de-duplicated (nested/covered names dropped) and capped
// (flush_max_commands), and commands are paced (flush_pause_ms).

const (
	flushProbeAttempts = 10
	flushProbeInterval = 2 * time.Second
	flushCmdTimeout    = 30 * time.Second
	flushAbortAfter    = 3 // consecutive failures before anything succeeded -> give up
)

type flushSettings struct {
	Command     string   // unbound-control binary
	Args        []string // extra leading args, e.g. -c /etc/unbound/unbound.conf
	DomainsDir  string   // announcer's domains.d ("" = use only communities.conf patterns)
	MaxCommands int
	Pause       time.Duration
}

type flushTargets struct {
	Zones []string // -> flush_zone
	Names []string // -> flush
}

func (t flushTargets) total() int { return len(t.Zones) + len(t.Names) }

type cmdRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

var flushLabelRE = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$`)

// validFlushName is deliberately strict: the name becomes an argv element of
// unbound-control, so anything that is not a plain hostname (leading '-',
// spaces, shell/control characters) is refused outright.
func validFlushName(n string) bool {
	if n == "" || len(n) > 253 {
		return false
	}
	for _, l := range strings.Split(n, ".") {
		if !flushLabelRE.MatchString(l) {
			return false
		}
	}
	return true
}

// readDomainPatterns reads every non-hidden *.list file in dir (same file
// rules as the Python announcer: '#' comments, one pattern per line).
func readDomainPatterns(dir string) (patterns []string, files int, err error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".list") || strings.HasPrefix(name, ".") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			return nil, files, rerr
		}
		files++
		for _, line := range strings.Split(string(b), "\n") {
			if i := strings.IndexByte(line, '#'); i >= 0 {
				line = line[:i]
			}
			if line = strings.TrimSpace(line); line != "" {
				patterns = append(patterns, line)
			}
		}
	}
	return patterns, files, nil
}

// buildFlushTargets turns name patterns into a minimal, validated command
// list. skipped counts patterns refused by validFlushName.
func buildFlushTargets(patterns []string) (t flushTargets, skipped int) {
	zoneSet := map[string]bool{}
	nameSet := map[string]bool{}
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(p), "."))
		if base, ok := strings.CutPrefix(p, "*."); ok {
			if validFlushName(base) {
				zoneSet[base] = true
			} else {
				skipped++
			}
			continue
		}
		if validFlushName(p) {
			nameSet[p] = true
		} else {
			skipped++
		}
	}
	covered := func(name string, set map[string]bool, strict bool) bool {
		for z := range set {
			if name == z && strict {
				continue
			}
			if name == z || strings.HasSuffix(name, "."+z) {
				return true
			}
		}
		return false
	}
	for z := range zoneSet {
		// a zone nested under another zone is redundant
		parentExists := false
		for o := range zoneSet {
			if o != z && strings.HasSuffix(z, "."+o) {
				parentExists = true
				break
			}
		}
		if !parentExists {
			t.Zones = append(t.Zones, z)
		}
	}
	for n := range nameSet {
		if !covered(n, zoneSet, false) {
			t.Names = append(t.Names, n)
		}
	}
	sort.Strings(t.Zones)
	sort.Strings(t.Names)
	return t, skipped
}

type flushResult struct {
	OK, Failed int
	Aborted    bool
	FirstErr   string
	Truncated  int // targets dropped by MaxCommands
}

func oneLine(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// runFlush executes the flush commands. It first waits for Unbound's
// control channel (`unbound-control status`), because the daemon can start
// before Unbound has finished starting. sleep is injectable for tests.
func runFlush(ctx context.Context, fs flushSettings, t flushTargets, run cmdRunner, sleep func(time.Duration), log logger) flushResult {
	var res flushResult
	base := append([]string(nil), fs.Args...)

	ready := false
	var probeErr string
	for i := 0; i < flushProbeAttempts && ctx.Err() == nil; i++ {
		pctx, cancel := context.WithTimeout(ctx, flushCmdTimeout)
		out, err := run(pctx, fs.Command, append(append([]string(nil), base...), "status")...)
		cancel()
		if err == nil {
			ready = true
			break
		}
		probeErr = fmt.Sprintf("%v: %s", err, oneLine(out))
		sleep(flushProbeInterval)
	}
	if !ready {
		res.Aborted, res.FirstErr = true, probeErr
		return res
	}

	type cmd struct{ verb, name string }
	var cmds []cmd
	for _, z := range t.Zones {
		cmds = append(cmds, cmd{"flush_zone", z})
	}
	for _, n := range t.Names {
		cmds = append(cmds, cmd{"flush", n})
	}
	if fs.MaxCommands > 0 && len(cmds) > fs.MaxCommands {
		res.Truncated = len(cmds) - fs.MaxCommands
		cmds = cmds[:fs.MaxCommands]
	}

	for _, c := range cmds {
		if ctx.Err() != nil {
			res.Aborted = true
			break
		}
		cctx, cancel := context.WithTimeout(ctx, flushCmdTimeout)
		out, err := run(cctx, fs.Command, append(append([]string(nil), base...), c.verb, c.name)...)
		cancel()
		if err != nil {
			res.Failed++
			if res.FirstErr == "" {
				res.FirstErr = fmt.Sprintf("%s %s: %v: %s", c.verb, c.name, err, oneLine(out))
			}
			if res.OK == 0 && res.Failed >= flushAbortAfter {
				res.Aborted = true
				break
			}
		} else {
			res.OK++
		}
		if fs.Pause > 0 {
			sleep(fs.Pause)
		}
	}
	return res
}

// flushSettingsFromConfig builds the flush settings from the [unbound] section.
func flushSettingsFromConfig(cfg Config) flushSettings {
	return flushSettings{
		Command:     cfg.UnboundControlCmd,
		Args:        strings.Fields(cfg.UnboundControlArgs),
		DomainsDir:  cfg.FlushDomainsDir,
		MaxCommands: cfg.FlushMaxCommands,
		Pause:       time.Duration(cfg.FlushPauseMS) * time.Millisecond,
	}
}

// gatherFlushPatterns collects the name patterns of the loaded
// communities.conf rules and, if domainsDir is non-empty, those of the
// announcer's *.list files (a read error there is logged and skipped).
func gatherFlushPatterns(rs *ruleSet, domainsDir string, log logger) []string {
	var patterns []string
	if rs != nil {
		for i := range rs.rules {
			patterns = append(patterns, rs.rules[i].patterns...)
		}
	}
	if domainsDir != "" {
		dp, files, err := readDomainPatterns(domainsDir)
		if err != nil {
			log.Warnf("unbound flush: cannot read %s, using communities.conf patterns only: %v", domainsDir, err)
		} else {
			log.Infof("unbound flush: %d pattern(s) from %d file(s) in %s", len(dp), files, domainsDir)
			patterns = append(patterns, dp...)
		}
	}
	return patterns
}

func recordFlush(res flushResult) {
	ctr.flushOK.Add(uint64(res.OK))
	ctr.flushFailed.Add(uint64(res.Failed))
	ctr.flushedAt.Store(time.Now().Unix())
}

// flushUnboundCache is the startup flush: the watched names (announcer's
// domains.d plus the loaded communities.conf patterns), flushed once, in its
// own goroutine, after the transport socket is listening.
func flushUnboundCache(ctx context.Context, fs flushSettings, rs *ruleSet, log logger) {
	patterns := gatherFlushPatterns(rs, fs.DomainsDir, log)
	t, skipped := buildFlushTargets(patterns)
	if skipped > 0 {
		log.Warnf("unbound flush: %d pattern(s) skipped (not a plain hostname)", skipped)
	}
	if t.total() == 0 {
		log.Infof("unbound flush: nothing to flush (no domain patterns found)")
		return
	}

	start := time.Now()
	res := runFlush(ctx, fs, t, execRunner, time.Sleep, log)
	recordFlush(res)
	switch {
	case res.Aborted && res.OK == 0:
		log.Warnf("unbound flush: gave up, unbound-control is not usable (check control_command / control_args and that Unbound's remote control is enabled and readable by this user): %s", res.FirstErr)
	default:
		log.Infof("unbound flush: %d zone flush(es) + %d name flush(es) planned, %d ok, %d failed, in %s",
			len(t.Zones), len(t.Names), res.OK, res.Failed, time.Since(start).Round(time.Millisecond))
		if res.Failed > 0 {
			log.Warnf("unbound flush: %d command(s) failed, first: %s", res.Failed, res.FirstErr)
		}
		if res.Truncated > 0 {
			log.Warnf("unbound flush: %d target(s) not flushed because flush_max_commands was reached", res.Truncated)
		}
	}
}
