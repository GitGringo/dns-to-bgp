package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"unbound-route-state/internal/bgp"
	"unbound-route-state/internal/state"
)

// The control socket: a UNIX socket, separate from the data socket the
// Python module writes to (so a slow `ctl routes` can never stall event
// ingest). Protocol: the client sends ONE line ("<command> [flags]\n"), the
// server writes the whole reply and closes. An error reply starts with
// "ERROR: ". Readable with plain `socat`/`nc -U`; `unbound-route-state ctl`
// is just a thin client for it (ctl.go).

const (
	controlMaxRequest = 1024
	controlTimeout    = 10 * time.Second
	jsonSchemaVersion = 1
)

// flushTotalTimeout bounds a whole `ctl flush`: it may issue hundreds of
// unbound-control calls, so it gets far more than controlTimeout.
const flushTotalTimeout = 10 * time.Minute

type controlServer struct {
	cfg     Config
	store   *state.Store
	rules   *atomic.Pointer[ruleSet]
	session func(context.Context) (bgp.SessionInfo, error)
	now     func() time.Time

	// `ctl flush` (flush.go). runner/sleeper default to the real ones;
	// tests inject fakes. flushing allows one flush at a time.
	flushCfg flushSettings
	runner   cmdRunner
	sleeper  func(time.Duration)
	flushing atomic.Bool
}

// serveControl runs until ctx is cancelled. An empty control_socket
// disables the feature (returns nil immediately).
func serveControl(ctx context.Context, s *controlServer, log logger) error {
	path := s.cfg.ControlSocket
	if path == "" {
		return nil
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("control: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, os.FileMode(s.cfg.ControlSocketMode)); err != nil {
		ln.Close()
		return fmt.Errorf("control: chmod %s: %w", path, err)
	}
	go func() {
		<-ctx.Done()
		ln.Close()
		_ = os.Remove(path)
	}()
	log.Infof("control: listening on %s (mode %o)", path, s.cfg.ControlSocketMode)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("control: accept: %w", err)
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *controlServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(controlTimeout))
	line, err := bufio.NewReader(io.LimitReader(conn, controlMaxRequest)).ReadString('\n')
	if err != nil && line == "" {
		return
	}
	fields := strings.Fields(line)
	budget := controlTimeout
	if len(fields) > 0 && fields[0] == "flush" && !wantsHelp(fields[1:]) {
		budget = flushTotalTimeout // many unbound-control calls; see flush.go
		_ = conn.SetDeadline(time.Now().Add(budget + time.Second))
	}
	rctx, cancel := context.WithTimeout(ctx, budget-time.Second)
	defer cancel()
	_, _ = conn.Write(s.dispatch(rctx, fields))
}

func errReply(format string, args ...any) []byte {
	return []byte("ERROR: " + fmt.Sprintf(format, args...) + "\n")
}

// helpText is the command overview, shared by the daemon (reply to "help")
// and the ctl client (which prints it locally, so --help works even when
// the daemon is not running).
const helpText = `commands:
  stats  [--json]                         counters: ingest, queue, routes, rules, BGP session
  routes [FLAGS]                          the live route cache
  route  IP [--json]                      one route in detail
  rules  [--json]                         loaded communities.conf blocks and their hit counts
  bgp    [--json]                         BGP session state
  flush  [NAME] [FLAGS]                   flush names from Unbound's cache (via unbound-control)
  help   [COMMAND]                        this overview, or details for one command

"<command> --help" (or "help <command>") shows the flags of a command.
`

var commandHelp = map[string]string{
	"stats": `stats [--json]

  Counters since the daemon started: process, ingest (Python module ->
  daemon), event queue, route cache, communities rules, BGP session.

  --json    machine-readable output (stable schema, "schema_version")
`,
	"routes": `routes [--ip IP] [--fqdn TEXT] [--sort KEY] [--limit N] [--json | --json-lines]

  The live route cache, one line per IP.

  --ip IP         only this address
  --fqdn TEXT     only routes held by a domain containing TEXT (case-insensitive)
  --sort KEY      ttl (default, soonest to lapse first) | age | ip | renewals
  --limit N       print at most N routes (0 = no limit)
  --json          one JSON document: {"total","returned","routes":[...]}
  --json-lines    one JSON object per line (for streaming processing)
  --history       reserved: recently withdrawn routes (not implemented yet)

  Columns: TTL_LEFT = time until it is withdrawn, AGE = time since the first
  announce, RENEWALS = events that hit this IP while it was cached, LAST_FLOW =
  how long ago a live flow to this IP was last seen in conntrack ("-" = never /
  keepalive off; a trailing * = the route is alive only because of that
  traffic, its DNS TTL would have ended it), NEXT_HOP = the per-rule next-hop
  for this route's address family ("-" = daemon default).
`,
	"route": `route IP [--json]

  One cached route in detail: domains, all attributes, announce/expiry
  times, renewals. Exit status 1 if the IP is not in the cache.

  --json    machine-readable output
`,
	"rules": `rules [--json]

  The loaded communities.conf blocks in file order, with how many events
  matched each since the current rule set was loaded (the counters restart
  on every hot-reload; "generation" tells which load you are looking at).
  The [default:missed] block is listed last as "default".

  --json    machine-readable output
`,
	"bgp": `bgp [--json]

  BGP session state: FSM state, uptime, flaps, message counters and the
  reason for the last disconnect.

  --json    machine-readable output
`,
	"flush": `flush [NAME] [--zone] [--with-domains-dir] [--dry-run] [--json]

  Removes names from Unbound's cache (through unbound-control, run by the
  daemon), so the next client query is resolved afresh and goes through the
  Python module again - which re-announces the route. Same mechanism as
  flush_on_start, run on demand.

  flush                 every pattern of the loaded communities.conf rules
  flush NAME            one name: "example.com" -> unbound-control flush
                        (exact name); "*.example.com" -> flush_zone (the
                        zone and everything below it)
  --zone                treat NAME as a zone: flush_zone NAME
  --with-domains-dir    (no NAME) also include the announcer's *.list files
                        from flush_domains_dir - the set flush_on_start uses
  --dry-run             print the unbound-control commands, run nothing
  --json                machine-readable result

  flush_zone walks the whole cache and is slow: the command count is capped
  by flush_max_commands and only one flush may run at a time. A flush that
  fails (even partly) exits with status 1.
`,
	"help": `help [COMMAND]

  Without arguments: the command overview. With a command name: its flags.
`,
}

// helpFor returns the help text for one command ("" name = overview).
func helpFor(name string) (string, bool) {
	if name == "" {
		return helpText, true
	}
	h, ok := commandHelp[name]
	return h, ok
}

func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			return true
		}
	}
	return false
}

func (s *controlServer) dispatch(ctx context.Context, args []string) []byte {
	if len(args) == 0 {
		return []byte(helpText)
	}
	cmd, rest := args[0], args[1:]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		name := ""
		if cmd == "help" && len(rest) > 0 {
			name = rest[0]
		}
		h, ok := helpFor(name)
		if !ok {
			return errReply("no such command %q\n\n%s", name, helpText)
		}
		return []byte(h)
	}
	if wantsHelp(rest) {
		if h, ok := helpFor(cmd); ok {
			return []byte(h)
		}
	}
	switch cmd {
	case "stats":
		return s.cmdStats(ctx, rest)
	case "routes":
		return s.cmdRoutes(ctx, rest, "")
	case "route":
		if len(rest) == 0 {
			return errReply("route: IP address required")
		}
		return s.cmdRoutes(ctx, rest[1:], rest[0])
	case "rules":
		return s.cmdRules(rest)
	case "bgp":
		return s.cmdBGP(ctx, rest)
	case "flush":
		return s.cmdFlush(ctx, rest)
	default:
		return errReply("unknown command %q (try: help)", cmd)
	}
}

// ---------------------------------------------------------------- flags

type opts struct {
	json, jsonLines bool
	ip, fqdn, sort  string
	limit           int
	history         bool
}

func parseOpts(args []string, allowed ...string) (opts, error) {
	var o opts
	ok := func(name string) bool {
		for _, a := range allowed {
			if a == name {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !ok(a) {
			return o, fmt.Errorf("unknown or unsupported flag %q", a)
		}
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		switch a {
		case "--json":
			o.json = true
		case "--json-lines":
			o.jsonLines = true
		case "--history":
			o.history = true
		case "--ip":
			o.ip, err = next()
		case "--fqdn":
			o.fqdn, err = next()
		case "--sort":
			o.sort, err = next()
		case "--limit":
			var v string
			if v, err = next(); err == nil {
				o.limit, err = strconv.Atoi(v)
				if err == nil && o.limit < 0 {
					err = fmt.Errorf("--limit must be >= 0")
				}
			}
		}
		if err != nil {
			return o, err
		}
	}
	return o, nil
}

// ------------------------------------------------------------- formatting

func fmtDur(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	case sec < 86400:
		return fmt.Sprintf("%dh%02dm", sec/3600, sec%3600/60)
	default:
		return fmt.Sprintf("%dd%02dh", sec/86400, sec%86400/3600)
	}
}

func fmtBytes(b uint64) string {
	const mib = 1 << 20
	if b >= mib {
		return fmt.Sprintf("%.1f MiB", float64(b)/mib)
	}
	return fmt.Sprintf("%.1f KiB", float64(b)/1024)
}

func fmtTime(unix int64) string {
	if unix <= 0 {
		return "-"
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// fmtIP renders an address in canonical (compressed) form; state.IP.String
// is for logs and prints IPv6 uncompressed.
func fmtIP(ip state.IP) string {
	if ip.IsIPv4() {
		return netip.AddrFrom4([4]byte{ip[12], ip[13], ip[14], ip[15]}).String()
	}
	return netip.AddrFrom16(ip).String()
}

func communityStrings(cs []state.Community) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%d:%d", c.ASN(), c.Value()))
	}
	return out
}

func largeStrings(cs []state.LargeCommunity) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%d:%d:%d", c.ASN, c.Data1, c.Data2))
	}
	return out
}

func asPathStrings(p []uint32) []string {
	out := make([]string, 0, len(p))
	for _, a := range p {
		out = append(out, strconv.FormatUint(uint64(a), 10))
	}
	return out
}

func jsonReply(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errReply("json: %v", err)
	}
	return append(b, '\n')
}

func kvTable(w io.Writer, rows [][2]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(tw, "  %s\t%s\n", r[0], r[1])
	}
	tw.Flush()
}

func u(v uint64) string { return strconv.FormatUint(v, 10) }
func i(v int64) string  { return strconv.FormatInt(v, 10) }

// ----------------------------------------------------------------- stats

type statsReport struct {
	SchemaVersion int    `json:"schema_version"`
	Time          string `json:"time"`
	Process       struct {
		Version        string `json:"version"`
		UptimeSeconds  int64  `json:"uptime_seconds"`
		Goroutines     int    `json:"goroutines"`
		HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
		SysBytes       uint64 `json:"sys_bytes"`
	} `json:"process"`
	Ingest struct {
		ConnsAccepted    uint64 `json:"conns_accepted"`
		ConnsRejected    uint64 `json:"conns_rejected_max_conns"`
		LinesReceived    uint64 `json:"lines_received"`
		LinesMalformed   uint64 `json:"lines_malformed"`
		LinesInvalid     uint64 `json:"lines_invalid"`
		RecordsSubmitted uint64 `json:"records_submitted"`
		RecordsInvalid   uint64 `json:"records_invalid"`
	} `json:"ingest"`
	Queue struct {
		Depth         int64 `json:"depth"`
		Capacity      int64 `json:"capacity"`
		HighWatermark int64 `json:"high_watermark"`
		Dropped       int64 `json:"dropped_total"`
	} `json:"queue"`
	Routes struct {
		Active          int64 `json:"active"`
		ActiveIPv4      int64 `json:"active_ipv4"` // -1 if the snapshot could not be taken
		ActiveIPv6      int64 `json:"active_ipv6"`
		MaxIPEntries    int   `json:"max_ip_entries"`
		EventsHandled   int64 `json:"events_handled"`
		Announced       int64 `json:"announced_total"`
		Withdrawn       int64 `json:"withdrawn_total"`
		Renewals        int64 `json:"renewals_total"`
		DomainJoins     int64 `json:"domain_joins_total"`
		RejectedAtCap   int64 `json:"rejected_at_cap_total"`
		TTLFloorApplied int64 `json:"ttl_floor_applied_total"`
		MinRouteTTL     int   `json:"min_route_ttl_seconds"`
	} `json:"routes"`
	Rules struct {
		Generation   uint64 `json:"generation"`
		LoadedAt     string `json:"loaded_at"`
		Blocks       int    `json:"blocks"`
		HasDefault   bool   `json:"has_default_block"`
		ReloadOK     uint64 `json:"reload_ok_total"`
		ReloadFailed uint64 `json:"reload_failed_total"`
		Unclassified uint64 `json:"unclassified_total"`
	} `json:"rules"`
	Keepalive struct {
		Enabled             bool   `json:"enabled"`
		PollIntervalSeconds int    `json:"poll_interval_seconds"`
		LeaseSeconds        int    `json:"lease_seconds"`
		MaxHoldSeconds      int    `json:"max_hold_seconds"`
		HeldRoutes          int64  `json:"held_routes"` // kept past their DNS expiry by observed traffic right now (-1 = unknown)
		RoutesExtendedTotal int64  `json:"routes_extended_total"`
		LastPollAt          string `json:"last_poll_at"` // "-" = no poll yet
		LastPollIPsWithFlow int64  `json:"last_poll_ips_with_flows"`
		PollsOK             uint64 `json:"polls_ok_total"`
		PollsFailed         uint64 `json:"polls_failed_total"`
		LastError           string `json:"last_error,omitempty"`
	} `json:"keepalive"`
	UnboundFlush struct {
		Enabled     bool   `json:"enabled"`
		RanAt       string `json:"ran_at"` // "-" = has not run (yet)
		OKTotal     uint64 `json:"commands_ok_total"`
		FailedTotal uint64 `json:"commands_failed_total"`
	} `json:"unbound_flush"`
	BGP struct {
		Available             bool   `json:"available"`
		Error                 string `json:"error,omitempty"`
		State                 string `json:"state,omitempty"`
		Established           bool   `json:"established"`
		UptimeSeconds         int64  `json:"uptime_seconds"`
		Flaps                 uint32 `json:"flaps_total"`
		DisconnectReason      string `json:"last_disconnect_reason,omitempty"`
		DisconnectMessage     string `json:"last_disconnect_message,omitempty"`
		UpdatesSent           uint64 `json:"updates_sent_total"`
		UpdatesReceived       uint64 `json:"updates_received_total"`
		WithdrawsSent         uint64 `json:"withdraw_updates_sent_total"`
		NotificationsSent     uint64 `json:"notifications_sent_total"`
		NotificationsReceived uint64 `json:"notifications_received_total"`
	} `json:"bgp"`
}

func (s *controlServer) familyCounts(ctx context.Context) (v4, v6, held int64) {
	// Snapshot with a filter that counts and rejects: nothing is copied.
	now := s.now().Unix()
	_, err := s.store.Snapshot(ctx, func(r state.RouteInfo) bool {
		if r.IP.IsIPv4() {
			v4++
		} else {
			v6++
		}
		if int64(r.HoldUntil) > now && r.HoldUntil > r.DNSExpiresAt {
			held++
		}
		return false
	})
	if err != nil {
		return -1, -1, -1
	}
	return v4, v6, held
}

func (s *controlServer) buildStats(ctx context.Context) *statsReport {
	var r statsReport
	r.SchemaVersion = jsonSchemaVersion
	r.Time = s.now().UTC().Format(time.RFC3339)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	r.Process.Version = version
	r.Process.UptimeSeconds = int64(s.now().Sub(ctr.startedAt).Seconds())
	r.Process.Goroutines = runtime.NumGoroutine()
	r.Process.HeapAllocBytes, r.Process.SysBytes = ms.HeapAlloc, ms.Sys

	r.Ingest.ConnsAccepted = ctr.connsAccepted.Load()
	r.Ingest.ConnsRejected = ctr.connsRejected.Load()
	r.Ingest.LinesReceived = ctr.linesReceived.Load()
	r.Ingest.LinesMalformed = ctr.linesMalformed.Load()
	r.Ingest.LinesInvalid = ctr.linesInvalid.Load()
	r.Ingest.RecordsSubmitted = ctr.recordsSubmitted.Load()
	r.Ingest.RecordsInvalid = ctr.recordsInvalid.Load()

	sv := s.store.StatsSnapshot()
	r.Queue.Depth, r.Queue.Capacity = sv.QueueDepth, sv.QueueCapacity
	r.Queue.HighWatermark, r.Queue.Dropped = sv.QueueHighWatermark, sv.DroppedEvents

	r.Routes.Active = sv.ActiveIPs
	r.Routes.ActiveIPv4, r.Routes.ActiveIPv6, r.Keepalive.HeldRoutes = s.familyCounts(ctx)
	r.Routes.MaxIPEntries = s.cfg.MaxIPEntries
	r.Routes.EventsHandled, r.Routes.Announced, r.Routes.Withdrawn = sv.EventsHandled, sv.Announces, sv.Withdraws
	r.Routes.Renewals, r.Routes.DomainJoins = sv.Renewals, sv.DomainJoins
	r.Routes.RejectedAtCap, r.Routes.TTLFloorApplied = sv.RejectedAtCap, sv.TTLFloorApplied
	r.Routes.MinRouteTTL = s.cfg.MinRouteTTLSeconds

	rs := s.rules.Load()
	r.Rules.Generation = ctr.rulesGeneration.Load()
	r.Rules.LoadedAt = fmtTime(ctr.rulesLoadedAt.Load())
	if rs != nil {
		r.Rules.Blocks = len(rs.rules)
		r.Rules.HasDefault = rs.defaultRule != nil
	}
	r.Rules.ReloadOK, r.Rules.ReloadFailed = ctr.rulesReloadOK.Load(), ctr.rulesReloadFailed.Load()
	r.Rules.Unclassified = ctr.unclassified.Load()

	r.Keepalive.Enabled = s.cfg.KeepaliveEnabled
	r.Keepalive.PollIntervalSeconds = s.cfg.KeepalivePollSeconds
	r.Keepalive.LeaseSeconds = s.cfg.KeepaliveLeaseSeconds
	r.Keepalive.MaxHoldSeconds = s.cfg.KeepaliveMaxHoldSeconds
	r.Keepalive.RoutesExtendedTotal = sv.TrafficExtended
	r.Keepalive.LastPollAt = fmtTime(ctr.kaLastPollAt.Load())
	r.Keepalive.LastPollIPsWithFlow = ctr.kaLastMatched.Load()
	r.Keepalive.PollsOK, r.Keepalive.PollsFailed = ctr.kaPollsOK.Load(), ctr.kaPollsFailed.Load()
	if e, ok := ctr.kaLastErr.Load().(string); ok {
		r.Keepalive.LastError = e
	}

	r.UnboundFlush.Enabled = s.cfg.FlushOnStart
	r.UnboundFlush.RanAt = fmtTime(ctr.flushedAt.Load())
	r.UnboundFlush.OKTotal, r.UnboundFlush.FailedTotal = ctr.flushOK.Load(), ctr.flushFailed.Load()

	if s.session != nil {
		if si, err := s.session(ctx); err != nil {
			r.BGP.Error = err.Error()
		} else {
			r.BGP.Available = true
			fillBGP(&r.BGP.State, &r.BGP.Established, &r.BGP.UptimeSeconds, &r.BGP.Flaps, si)
			r.BGP.DisconnectReason, r.BGP.DisconnectMessage = si.DisconnectReason, si.DisconnectMessage
			r.BGP.UpdatesSent, r.BGP.UpdatesReceived, r.BGP.WithdrawsSent = si.UpdatesSent, si.UpdatesReceived, si.WithdrawsSent
			r.BGP.NotificationsSent, r.BGP.NotificationsReceived = si.NotificationsSent, si.NotificationsReceived
		}
	} else {
		r.BGP.Error = "not available"
	}
	return &r
}

func fillBGP(st *string, est *bool, up *int64, flaps *uint32, si bgp.SessionInfo) {
	*st, *est, *up, *flaps = si.State, si.Established, si.UptimeSeconds, si.Flaps
}

func (s *controlServer) cmdStats(ctx context.Context, args []string) []byte {
	o, err := parseOpts(args, "--json")
	if err != nil {
		return errReply("stats: %v", err)
	}
	r := s.buildStats(ctx)
	if o.json {
		return jsonReply(r)
	}
	var b strings.Builder
	sec := func(name string, rows [][2]string) {
		fmt.Fprintf(&b, "%s\n", name)
		kvTable(&b, rows)
		b.WriteString("\n")
	}
	sec("process", [][2]string{
		{"version", r.Process.Version},
		{"uptime", fmtDur(r.Process.UptimeSeconds)},
		{"goroutines", strconv.Itoa(r.Process.Goroutines)},
		{"heap_alloc", fmtBytes(r.Process.HeapAllocBytes)},
		{"sys", fmtBytes(r.Process.SysBytes)},
	})
	sec("ingest", [][2]string{
		{"conns_accepted", u(r.Ingest.ConnsAccepted)}, {"conns_rejected_max_conns", u(r.Ingest.ConnsRejected)},
		{"lines_received", u(r.Ingest.LinesReceived)}, {"lines_malformed", u(r.Ingest.LinesMalformed)},
		{"lines_invalid", u(r.Ingest.LinesInvalid)},
		{"records_submitted", u(r.Ingest.RecordsSubmitted)}, {"records_invalid", u(r.Ingest.RecordsInvalid)},
	})
	sec("queue", [][2]string{
		{"depth", fmt.Sprintf("%d / %d", r.Queue.Depth, r.Queue.Capacity)},
		{"high_watermark", i(r.Queue.HighWatermark)}, {"dropped_total", i(r.Queue.Dropped)},
	})
	sec("routes", [][2]string{
		{"active", fmt.Sprintf("%d (ipv4 %d, ipv6 %d) of max %d", r.Routes.Active, r.Routes.ActiveIPv4, r.Routes.ActiveIPv6, r.Routes.MaxIPEntries)},
		{"events_handled", i(r.Routes.EventsHandled)},
		{"announced_total", i(r.Routes.Announced)}, {"withdrawn_total", i(r.Routes.Withdrawn)},
		{"renewals_total", i(r.Routes.Renewals)}, {"domain_joins_total", i(r.Routes.DomainJoins)},
		{"rejected_at_cap_total", i(r.Routes.RejectedAtCap)},
		{"ttl_floor_applied_total", fmt.Sprintf("%d (min_route_ttl_seconds=%d)", r.Routes.TTLFloorApplied, r.Routes.MinRouteTTL)},
	})
	sec("rules", [][2]string{
		{"generation", fmt.Sprintf("%d (loaded %s)", r.Rules.Generation, r.Rules.LoadedAt)},
		{"blocks", fmt.Sprintf("%d (default block: %v)", r.Rules.Blocks, r.Rules.HasDefault)},
		{"reload_ok / failed", fmt.Sprintf("%d / %d", r.Rules.ReloadOK, r.Rules.ReloadFailed)},
		{"unclassified_total", u(r.Rules.Unclassified)},
	})
	if r.Keepalive.Enabled {
		sec("keepalive", [][2]string{
			{"enabled", fmt.Sprintf("yes (poll %ds, lease %ds, max hold %ds)", r.Keepalive.PollIntervalSeconds, r.Keepalive.LeaseSeconds, r.Keepalive.MaxHoldSeconds)},
			{"held_routes", fmt.Sprintf("%d (kept past DNS expiry by traffic right now)", r.Keepalive.HeldRoutes)},
			{"routes_extended_total", i(r.Keepalive.RoutesExtendedTotal)},
			{"last_poll", fmt.Sprintf("%s, %d cached IP(s) with live flows", r.Keepalive.LastPollAt, r.Keepalive.LastPollIPsWithFlow)},
			{"polls ok / failed", fmt.Sprintf("%d / %d", r.Keepalive.PollsOK, r.Keepalive.PollsFailed)},
			{"last_error", dash(r.Keepalive.LastError)},
		})
	} else {
		sec("keepalive", [][2]string{{"enabled", "no"}})
	}
	sec("unbound_flush", [][2]string{
		{"flush_on_start", fmt.Sprintf("%v", r.UnboundFlush.Enabled)},
		{"ran_at", r.UnboundFlush.RanAt},
		{"commands ok / failed", fmt.Sprintf("%d / %d", r.UnboundFlush.OKTotal, r.UnboundFlush.FailedTotal)},
	})
	b.WriteString("bgp\n")
	kvTable(&b, bgpRows(r))
	return []byte(b.String())
}

func bgpRows(r *statsReport) [][2]string {
	if !r.BGP.Available {
		return [][2]string{{"state", "unavailable: " + r.BGP.Error}}
	}
	rows := [][2]string{
		{"state", r.BGP.State},
		{"uptime", fmtDur(r.BGP.UptimeSeconds)},
		{"flaps_total", strconv.FormatUint(uint64(r.BGP.Flaps), 10)},
		{"updates sent / received", fmt.Sprintf("%d / %d", r.BGP.UpdatesSent, r.BGP.UpdatesReceived)},
		{"withdraw_updates_sent", u(r.BGP.WithdrawsSent)},
		{"notifications sent / received", fmt.Sprintf("%d / %d", r.BGP.NotificationsSent, r.BGP.NotificationsReceived)},
	}
	if r.BGP.DisconnectReason != "" {
		rows = append(rows, [2]string{"last_disconnect", dash(r.BGP.DisconnectReason) + " " + r.BGP.DisconnectMessage})
	}
	return rows
}

func (s *controlServer) cmdBGP(ctx context.Context, args []string) []byte {
	o, err := parseOpts(args, "--json")
	if err != nil {
		return errReply("bgp: %v", err)
	}
	r := s.buildStats(ctx)
	if o.json {
		return jsonReply(map[string]any{"schema_version": jsonSchemaVersion, "time": r.Time, "bgp": r.BGP})
	}
	var b strings.Builder
	kvTable(&b, bgpRows(r))
	return []byte(b.String())
}

// ---------------------------------------------------------------- routes

type routeJSON struct {
	IP               string   `json:"ip"`
	Family           string   `json:"family"`
	FQDNs            []string `json:"fqdns"`
	Communities      []string `json:"communities"`
	LargeCommunities []string `json:"large_communities"`
	NextHop          string   `json:"next_hop,omitempty"`
	NextHopV6        string   `json:"next_hop_v6,omitempty"`
	ASPath           []string `json:"as_path"`
	LocalPref        *uint32  `json:"local_pref"`
	MED              *uint32  `json:"med"`
	Domains          int      `json:"domains"`
	AnnouncedAt      string   `json:"announced_at"`
	AgeSeconds       int64    `json:"age_seconds"`
	ExpiresAt        string   `json:"expires_at"`
	TTLLeftSeconds   int64    `json:"ttl_left_seconds"`
	Renewals         uint32   `json:"renewals"`
	LastTrafficAt    string   `json:"last_traffic_at,omitempty"`
	LastTrafficAgo   *int64   `json:"last_traffic_ago_seconds"`
	HeldByTraffic    bool     `json:"held_by_traffic"` // alive only because traffic was seen (past its DNS expiry)
	DNSExpiresAt     string   `json:"dns_expires_at"`
}

func toRouteJSON(r state.RouteInfo, now int64) routeJSON {
	fam := "ipv6"
	if r.IP.IsIPv4() {
		fam = "ipv4"
	}
	j := routeJSON{
		IP: fmtIP(r.IP), Family: fam,
		FQDNs:            append([]string{}, r.FQDNs...),
		Communities:      communityStrings(r.Attrs.Communities),
		LargeCommunities: largeStrings(r.Attrs.LargeCommunities),
		NextHop:          r.Attrs.NextHop,
		NextHopV6:        r.Attrs.NextHopV6,
		ASPath:           asPathStrings(r.Attrs.ASPath),
		Domains:          r.DomainCount,
		AnnouncedAt:      fmtTime(int64(r.AnnouncedAt)),
		AgeSeconds:       now - int64(r.AnnouncedAt),
		ExpiresAt:        fmtTime(int64(r.ExpiresAt)),
		TTLLeftSeconds:   int64(r.ExpiresAt) - now,
		Renewals:         r.Renewals,
		HeldByTraffic:    int64(r.HoldUntil) > now && r.HoldUntil > r.DNSExpiresAt,
		DNSExpiresAt:     fmtTime(int64(r.DNSExpiresAt)),
	}
	if r.LastTraffic > 0 {
		ago := now - int64(r.LastTraffic)
		j.LastTrafficAt, j.LastTrafficAgo = fmtTime(int64(r.LastTraffic)), &ago
	}
	if r.Attrs.HasLocalPref {
		v := r.Attrs.LocalPref
		j.LocalPref = &v
	}
	if r.Attrs.HasMED {
		v := r.Attrs.MED
		j.MED = &v
	}
	return j
}

func (s *controlServer) cmdRoutes(ctx context.Context, args []string, exactIP string) []byte {
	cmdName := "routes"
	allowed := []string{"--json", "--json-lines", "--ip", "--fqdn", "--sort", "--limit", "--history"}
	if exactIP != "" {
		cmdName = "route"
		allowed = []string{"--json"}
	}
	o, err := parseOpts(args, allowed...)
	if err != nil {
		return errReply("%s: %v", cmdName, err)
	}
	if o.history {
		return errReply("%s: --history is not implemented yet (the withdraw hook for it exists: state.Config.OnWithdraw)", cmdName)
	}
	if exactIP != "" {
		o.ip = exactIP
	}
	var wantIP state.IP
	hasIP := false
	if o.ip != "" {
		a, err := netip.ParseAddr(o.ip)
		if err != nil {
			return errReply("%s: bad IP %q", cmdName, o.ip)
		}
		if a.Is4() {
			b := a.As4()
			wantIP = state.IPv4ToIP(b[0], b[1], b[2], b[3])
		} else {
			wantIP = state.IPv6ToIP(a.As16())
		}
		hasIP = true
	}
	switch o.sort {
	case "", "ttl", "age", "ip", "renewals":
	default:
		return errReply("%s: --sort must be ttl, age, ip or renewals", cmdName)
	}
	wantFQDN := strings.ToLower(o.fqdn)

	routes, err := s.store.Snapshot(ctx, func(r state.RouteInfo) bool {
		if hasIP && r.IP != wantIP {
			return false
		}
		if wantFQDN != "" {
			hit := false
			for _, f := range r.FQDNs {
				if strings.Contains(strings.ToLower(f), wantFQDN) {
					hit = true
					break
				}
			}
			if !hit {
				return false
			}
		}
		return true
	})
	if err != nil {
		return errReply("%s: snapshot: %v", cmdName, err)
	}

	sortRoutes(routes, o.sort)
	total := len(routes)
	if o.limit > 0 && len(routes) > o.limit {
		routes = routes[:o.limit]
	}
	now := s.now().Unix()

	if exactIP != "" {
		if len(routes) == 0 {
			return errReply("route: %s is not in the cache", exactIP)
		}
		if o.json {
			return jsonReply(map[string]any{"schema_version": jsonSchemaVersion, "route": toRouteJSON(routes[0], now)})
		}
		return []byte(routeDetail(toRouteJSON(routes[0], now)))
	}

	if o.jsonLines {
		var b strings.Builder
		for _, r := range routes {
			line, _ := json.Marshal(toRouteJSON(r, now))
			b.Write(line)
			b.WriteByte('\n')
		}
		return []byte(b.String())
	}
	if o.json {
		js := make([]routeJSON, 0, len(routes))
		for _, r := range routes {
			js = append(js, toRouteJSON(r, now))
		}
		return jsonReply(map[string]any{
			"schema_version": jsonSchemaVersion, "time": s.now().UTC().Format(time.RFC3339),
			"total": total, "returned": len(js), "routes": js,
		})
	}

	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "IP\tTTL_LEFT\tAGE\tRENEWALS\tLAST_FLOW\tCOMMUNITIES\tNEXT_HOP\tLP\tMED\tFQDN")
	for _, r := range routes {
		j := toRouteJSON(r, now)
		lp, med := "-", "-"
		if j.LocalPref != nil {
			lp = strconv.FormatUint(uint64(*j.LocalPref), 10)
		}
		if j.MED != nil {
			med = strconv.FormatUint(uint64(*j.MED), 10)
		}
		comm := append(append([]string{}, j.Communities...), j.LargeCommunities...)
		// FQDN is the last column on purpose: it is the only one of variable
		// (and possibly large) width, so the numeric columns stay aligned.
		lastFlow := "-"
		if j.LastTrafficAgo != nil {
			lastFlow = fmtDur(*j.LastTrafficAgo)
			if j.HeldByTraffic {
				lastFlow += "*" // alive only thanks to the traffic
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			j.IP, fmtDur(j.TTLLeftSeconds), fmtDur(j.AgeSeconds), j.Renewals, lastFlow,
			dash(strings.Join(comm, ",")), dash(familyNextHop(j)), lp, med, dash(strings.Join(j.FQDNs, ",")))
	}
	tw.Flush()
	fmt.Fprintf(&b, "# %d of %d route(s)\n", len(routes), total)
	return []byte(b.String())
}

func sortRoutes(rs []state.RouteInfo, by string) {
	less := func(a, b state.RouteInfo) bool { return a.ExpiresAt < b.ExpiresAt } // default = ttl: soonest to lapse first
	switch by {
	case "age":
		less = func(a, b state.RouteInfo) bool { return a.AnnouncedAt < b.AnnouncedAt }
	case "ip":
		less = func(a, b state.RouteInfo) bool { return string(a.IP[:]) < string(b.IP[:]) }
	case "renewals":
		less = func(a, b state.RouteInfo) bool { return a.Renewals > b.Renewals }
	}
	sort.SliceStable(rs, func(x, y int) bool {
		if less(rs[x], rs[y]) {
			return true
		}
		if less(rs[y], rs[x]) {
			return false
		}
		return string(rs[x].IP[:]) < string(rs[y].IP[:]) // stable, deterministic order
	})
}

// familyNextHop is the per-rule next-hop override that applies to this
// route's own family (the one the BGP layer will use; "-" = the daemon's
// configured default for that family).
func familyNextHop(j routeJSON) string {
	if j.Family == "ipv4" {
		return j.NextHop
	}
	if j.NextHopV6 != "" {
		return j.NextHopV6
	}
	if a, err := netip.ParseAddr(j.NextHop); err == nil && a.Is6() {
		return j.NextHop // legacy: IPv6 literal in next_hop
	}
	return ""
}

func lastTrafficText(j routeJSON) string {
	if j.LastTrafficAgo == nil {
		return "-"
	}
	return fmt.Sprintf("%s (%s ago)", j.LastTrafficAt, fmtDur(*j.LastTrafficAgo))
}

func routeDetail(j routeJSON) string {
	var b strings.Builder
	lp, med := "-", "-"
	if j.LocalPref != nil {
		lp = strconv.FormatUint(uint64(*j.LocalPref), 10)
	}
	if j.MED != nil {
		med = strconv.FormatUint(uint64(*j.MED), 10)
	}
	fmt.Fprintf(&b, "%s/%s\n", j.IP, map[string]string{"ipv4": "32", "ipv6": "128"}[j.Family])
	kvTable(&b, [][2]string{
		{"fqdns", dash(strings.Join(j.FQDNs, ", "))},
		{"domains", strconv.Itoa(j.Domains)},
		{"communities", dash(strings.Join(j.Communities, ", "))},
		{"large_communities", dash(strings.Join(j.LargeCommunities, ", "))},
		{"next_hop", dash(j.NextHop)},
		{"next_hop_v6", dash(j.NextHopV6)},
		{"next_hop (applies)", dash(familyNextHop(j)) + "  (- = daemon default for this family)"},
		{"as_path", dash(strings.Join(j.ASPath, " "))},
		{"local_pref", lp},
		{"med", med},
		{"announced_at", fmt.Sprintf("%s (%s ago)", j.AnnouncedAt, fmtDur(j.AgeSeconds))},
		{"expires_at", fmt.Sprintf("%s (in %s)", j.ExpiresAt, fmtDur(j.TTLLeftSeconds))},
		{"renewals", strconv.FormatUint(uint64(j.Renewals), 10)},
		{"dns_expires_at", j.DNSExpiresAt},
		{"last_traffic", lastTrafficText(j)},
		{"held_by_traffic", fmt.Sprintf("%v", j.HeldByTraffic)},
	})
	return b.String()
}

// ----------------------------------------------------------------- rules

type listFileJSON struct {
	Name         string `json:"name"`
	Patterns     int    `json:"patterns"` // valid patterns read from the file
	Exists       bool   `json:"exists"`
	InvalidLines int    `json:"invalid_lines"`
}

type ruleJSON struct {
	Index            int            `json:"index"`         // -1 = the [default:missed] block
	Patterns         []string       `json:"patterns"`      // inline patterns of the header
	ListFiles        []listFileJSON `json:"list_files"`    // domains.d files referenced by the header
	PatternCount     int            `json:"pattern_count"` // effective total: inline + all list files
	Communities      []string       `json:"communities"`
	LargeCommunities []string       `json:"large_communities"`
	NextHop          string         `json:"next_hop,omitempty"`
	NextHopV6        string         `json:"next_hop_v6,omitempty"`
	ASPath           []string       `json:"as_path"`
	LocalPref        *uint32        `json:"local_pref"`
	MED              *uint32        `json:"med"`
	Hits             uint64         `json:"hits"`
}

func toRuleJSON(idx int, r *communityRule, hits uint64, rs *ruleSet) ruleJSON {
	toks := r.header
	if len(toks) == 0 {
		toks = r.patterns
	}
	inline, files := []string{}, []listFileJSON{}
	for _, t := range toks {
		if len(r.header) > 0 && isListRef(t) {
			ref, ok := rs.listRefByName(t)
			files = append(files, listFileJSON{Name: t, Patterns: ref.patterns, Exists: ok && ref.exists, InvalidLines: ref.invalid})
		} else {
			inline = append(inline, t)
		}
	}
	j := ruleJSON{
		Index: idx, Patterns: inline, ListFiles: files, PatternCount: len(r.patterns),
		Communities:      communityStrings(r.communities),
		LargeCommunities: largeStrings(r.largeCommunities),
		NextHop:          r.nextHop, NextHopV6: r.nextHopV6, ASPath: asPathStrings(r.asPath), Hits: hits,
	}
	if r.hasLocalPref {
		v := r.localPref
		j.LocalPref = &v
	}
	if r.hasMED {
		v := r.med
		j.MED = &v
	}
	return j
}

func (s *controlServer) cmdRules(args []string) []byte {
	o, err := parseOpts(args, "--json")
	if err != nil {
		return errReply("rules: %v", err)
	}
	rs := s.rules.Load()
	var list []ruleJSON
	if rs != nil {
		for k := range rs.rules {
			var h uint64
			if k < len(rs.hits) {
				h = rs.hits[k].Load()
			}
			list = append(list, toRuleJSON(k, &rs.rules[k], h, rs))
		}
		if rs.defaultRule != nil {
			list = append(list, toRuleJSON(-1, rs.defaultRule, rs.defaultHits.Load(), rs))
		}
	}
	gen, at := ctr.rulesGeneration.Load(), fmtTime(ctr.rulesLoadedAt.Load())
	if o.json {
		if list == nil {
			list = []ruleJSON{}
		}
		return jsonReply(map[string]any{
			"schema_version": jsonSchemaVersion, "generation": gen, "loaded_at": at,
			"note": "hits are counted since this generation was loaded", "rules": list,
		})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "generation %d, loaded %s (hits count since then)\n", gen, at)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tHITS\tPATTERNS\tCOMMUNITIES\tNEXT_HOP\tNEXT_HOP_V6\tAS_PATH\tLP\tMED")
	for _, r := range list {
		shown := append([]string{}, r.Patterns...)
		for _, f := range r.ListFiles {
			if f.Exists {
				shown = append(shown, fmt.Sprintf("%s(%d)", f.Name, f.Patterns))
			} else {
				shown = append(shown, f.Name+"(missing)")
			}
		}
		idx, pats := strconv.Itoa(r.Index), strings.Join(shown, ",")
		if r.Index < 0 {
			idx, pats = "default", "[default:missed]"
		}
		lp, med := "-", "-"
		if r.LocalPref != nil {
			lp = strconv.FormatUint(uint64(*r.LocalPref), 10)
		}
		if r.MED != nil {
			med = strconv.FormatUint(uint64(*r.MED), 10)
		}
		comm := append(append([]string{}, r.Communities...), r.LargeCommunities...)
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", idx, r.Hits, pats,
			dash(strings.Join(comm, ",")), dash(r.NextHop), dash(r.NextHopV6), dash(strings.Join(r.ASPath, " ")), lp, med)
	}
	tw.Flush()
	return []byte(b.String())
}

// ----------------------------------------------------------------- flush

type flushReport struct {
	SchemaVersion int      `json:"schema_version"`
	DryRun        bool     `json:"dry_run"`
	Zones         []string `json:"zones"`
	Names         []string `json:"names"`
	OK            int      `json:"commands_ok"`
	Failed        int      `json:"commands_failed"`
	Aborted       bool     `json:"aborted"`
	Truncated     int      `json:"not_flushed_max_commands"`
	FirstError    string   `json:"first_error,omitempty"`
	DurationMS    int64    `json:"duration_ms"`
}

func (s *controlServer) cmdFlush(ctx context.Context, args []string) []byte {
	var (
		name                                    string
		zone, withDomains, dryRun, asJSON, have bool
	)
	for _, a := range args {
		switch a {
		case "--zone":
			zone = true
		case "--with-domains-dir":
			withDomains = true
		case "--dry-run":
			dryRun = true
		case "--json":
			asJSON = true
		default:
			if strings.HasPrefix(a, "-") {
				return errReply("flush: unknown flag %q (try: flush --help)", a)
			}
			if have {
				return errReply("flush: at most one NAME (got %q and %q)", name, a)
			}
			name, have = a, true
		}
	}
	if zone && !have {
		return errReply("flush: --zone needs a NAME")
	}
	if withDomains && have {
		return errReply("flush: --with-domains-dir only applies when no NAME is given")
	}

	var patterns []string
	if have {
		p := strings.ToLower(strings.TrimSuffix(name, "."))
		if zone && !strings.HasPrefix(p, "*.") {
			p = "*." + p
		}
		patterns = []string{p}
	} else {
		dir := ""
		if withDomains {
			dir = s.flushCfg.DomainsDir
		}
		patterns = gatherFlushPatterns(s.rules.Load(), dir, nullLogger{})
	}
	t, skipped := buildFlushTargets(patterns)
	if have && skipped > 0 {
		return errReply("flush: %q is not a plain hostname (or *.hostname)", name)
	}
	if t.total() == 0 {
		return errReply("flush: nothing to flush (no domain patterns found)")
	}

	rep := flushReport{SchemaVersion: jsonSchemaVersion, DryRun: dryRun, Zones: nonNil(t.Zones), Names: nonNil(t.Names)}
	if dryRun {
		return flushReply(rep, asJSON, t)
	}

	if !s.flushing.CompareAndSwap(false, true) {
		return errReply("flush: another flush is already running")
	}
	defer s.flushing.Store(false)

	run, sleep := s.runner, s.sleeper
	if run == nil {
		run = execRunner
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	start := time.Now()
	res := runFlush(ctx, s.flushCfg, t, run, sleep, nullLogger{})
	recordFlush(res)
	rep.OK, rep.Failed, rep.Aborted, rep.Truncated, rep.FirstError = res.OK, res.Failed, res.Aborted, res.Truncated, res.FirstErr
	rep.DurationMS = time.Since(start).Milliseconds()
	return flushReply(rep, asJSON, t)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// nullLogger drops messages: ctl flush reports through its reply instead.
type nullLogger struct{}

func (nullLogger) Warnf(string, ...any) {}
func (nullLogger) Infof(string, ...any) {}

func flushReply(rep flushReport, asJSON bool, t flushTargets) []byte {
	failed := rep.Failed > 0 || (rep.Aborted && !rep.DryRun)
	if asJSON {
		out := jsonReply(rep)
		if failed {
			return append([]byte("ERROR: flush failed (details in the JSON below)\n"), out...)
		}
		return out
	}
	var b strings.Builder
	if failed {
		fmt.Fprintf(&b, "ERROR: flush failed: %s\n", dash(rep.FirstError))
	}
	verb := "will run"
	if !rep.DryRun {
		verb = "planned"
	}
	fmt.Fprintf(&b, "%d zone flush(es) + %d name flush(es) %s\n", len(t.Zones), len(t.Names), verb)
	for _, z := range t.Zones {
		fmt.Fprintf(&b, "  flush_zone %s\n", z)
	}
	for _, n := range t.Names {
		fmt.Fprintf(&b, "  flush      %s\n", n)
	}
	if rep.DryRun {
		b.WriteString("(dry run: nothing was executed)\n")
		return []byte(b.String())
	}
	fmt.Fprintf(&b, "result: %d ok, %d failed, %d dropped by flush_max_commands, in %dms\n", rep.OK, rep.Failed, rep.Truncated, rep.DurationMS)
	return []byte(b.String())
}
