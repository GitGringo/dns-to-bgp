// Command unbound-route-state is a DNS-to-BGP daemon. It receives resolved
// A/AAAA records from the Unbound Python module over a UNIX socket (NDJSON),
// classifies each name into BGP communities with a wildcard rule file,
// tracks route lifetimes in internal/state, and announces the routes to a
// BGP peer (for example BIRD) through internal/bgp.
//
// It has three subcommands: daemon (run the service), ctl (query a running
// daemon over its control socket) and version. This file holds the logger,
// the subcommand dispatcher and the daemon's start-up wiring.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/syslog"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"unbound-route-state/internal/bgp"
	"unbound-route-state/internal/state"
)

// stderrSyslogLogger logs to stderr and to the local syslog daemon. If
// /dev/log is unreachable it silently falls back to stderr only. It
// satisfies both state.Logger and the local `logger` interface in
// transport.go, which are structurally identical.
type stderrSyslogLogger struct {
	stderr *log.Logger
	sys    *syslog.Writer // nil if /dev/log wasn't reachable
}

// newLogger returns a stderrSyslogLogger, connecting to syslog if possible.
func newLogger() *stderrSyslogLogger {
	l := &stderrSyslogLogger{stderr: log.New(os.Stderr, "unbound-route-state: ", log.LstdFlags)}
	if w, err := syslog.New(syslog.LOG_DAEMON, "unbound-route-state"); err == nil {
		l.sys = w
	}
	return l
}

// Warnf logs a warning to stderr and syslog.
func (l *stderrSyslogLogger) Warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	l.stderr.Println("WARN:", msg)
	if l.sys != nil {
		_ = l.sys.Warning(msg)
	}
}

// Infof logs an informational message to stderr and syslog.
func (l *stderrSyslogLogger) Infof(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	l.stderr.Println("INFO:", msg)
	if l.sys != nil {
		_ = l.sys.Info(msg)
	}
}

// effectiveMinTTL is the route lifetime floor handed to the Store. With the
// traffic keepalive on it is at least twice the poll interval: a route whose
// DNS TTL is shorter than one poll cycle would otherwise lapse before the
// poller ever looked at it, and a flow that started right after the DNS answer
// would go unprotected. The floor only shapes the internal expiry schedule
// (as min_route_ttl_seconds always did), never what is announced.
func effectiveMinTTL(cfg Config) time.Duration {
	floor := time.Duration(cfg.MinRouteTTLSeconds) * time.Second
	if cfg.KeepaliveEnabled {
		if probe := 2 * time.Duration(cfg.KeepalivePollSeconds) * time.Second; probe > floor {
			floor = probe
		}
	}
	return floor
}

// keepLease is the Store's keepalive lease: 0 (off) unless [keepalive] is enabled.
func keepLease(cfg Config) time.Duration {
	if !cfg.KeepaliveEnabled {
		return 0
	}
	return time.Duration(cfg.KeepaliveLeaseSeconds) * time.Second
}

// version is the daemon's release number.
const version = "2.0.0"

// versionLine returns the one-line version banner.
func versionLine() string { return "unbound-route-state " + version }

// usageText is the top-level help shown by `help` and on usage errors.
const usageText = `DNS-to-BGP daemon: receives resolved A/AAAA records from the Unbound Python
module and announces them to BIRD as /32 (/128) BGP routes for the lifetime of
their DNS TTL.

usage:
  unbound-route-state daemon [options]          run the daemon
  unbound-route-state ctl [options] <command>   query a running daemon
                                                (stats, routes, route, rules, bgp, flush)
  unbound-route-state version                   print the version
  unbound-route-state help                      this help

more:
  unbound-route-state daemon --help             daemon options
  unbound-route-state ctl --help                ctl options and commands
  unbound-route-state ctl <command> --help      flags of one command
`

// printHelp writes the version banner and the top-level usage text to w.
func printHelp(w io.Writer) {
	fmt.Fprintf(w, "%s\n\n%s", versionLine(), usageText)
}

func main() { os.Exit(dispatch(os.Args[1:])) }

// dispatch routes the first argument to a subcommand. With no arguments it
// prints the help and the version.
func dispatch(args []string) int {
	if len(args) == 0 {
		printHelp(os.Stdout)
		return 0
	}
	switch args[0] {
	case "daemon":
		return runDaemon(args[1:])
	case "ctl":
		return runCtl(args[1:])
	case "version", "--version", "-version", "-V":
		fmt.Println(versionLine())
		return 0
	case "help", "-h", "--help", "-help":
		printHelp(os.Stdout)
		return 0
	}
	if strings.HasPrefix(args[0], "-") {
		// Pre-2.0 invocation (`unbound-route-state -config X`), e.g. an old
		// systemd unit: keep working, but say how it is spelled now.
		fmt.Fprintln(os.Stderr, "unbound-route-state: starting the daemon without the 'daemon' subcommand is deprecated; use: unbound-route-state daemon [options]")
		return runDaemon(args)
	}
	fmt.Fprintf(os.Stderr, "unbound-route-state: unknown command %q\n\n", args[0])
	printHelp(os.Stderr)
	return 2
}

// runDaemon parses the daemon options, loads the configuration and community
// rules, starts the BGP session and all background workers, and blocks until
// SIGINT or SIGTERM. It returns the process exit code.
func runDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "path to config file")
	debug := fs.Bool("debug", false, "log ANNOUNCE/WITHDRAW trace messages (domain, TTL) and GoBGP debug output (connection attempts, FSM)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "%s\n\nusage: unbound-route-state daemon [options]\n\nRuns the daemon in the foreground (systemd runs it this way).\n\noptions:\n", versionLine())
		fs.PrintDefaults()
	}
	if wantsHelp(args) {
		fs.SetOutput(os.Stdout) // an explicit --help is not an error: stdout, exit 0
		fs.Usage()
		return 0
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unbound-route-state daemon: unexpected argument %q\n\n", fs.Arg(0))
		fs.Usage()
		return 2
	}

	baseLog := newLogger()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// All components share one rate-limited logger (log_rate_limit_per_min).
	appLog := newRateLimitedLogger(baseLog, cfg.LogRateLimitPerMin)

	// Fingerprint of the rules file BEFORE the load, so a change landing in
	// between is picked up by the watcher's first tick instead of being
	// missed. (The list files' fingerprints are recorded by the loader itself,
	// each one before it reads it.)
	loadOptions := loadOpts{DomainsDir: cfg.DomainsDir, MaxFilePatterns: cfg.MaxDomainPatterns}
	rulesMainFP, _ := statFP(cfg.RulesFile)
	loaded, err := loadCommunityRulesFull(cfg.RulesFile, loadOptions)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rules, defaultRule := loaded.rules, loaded.def
	var ruleHolder atomic.Pointer[ruleSet]
	ruleHolder.Store(buildRuleSet(loaded))
	ctr.noteRulesLoaded()
	if len(rules) == 0 && defaultRule == nil {
		appLog.Warnf("main: no community rules loaded from %s - every route will carry no community", cfg.RulesFile)
	}

	warnASPathLoops(ruleHolder.Load(), cfg.NeighborASN, appLog)
	warnNextHopFamilies(ruleHolder.Load(), appLog)
	warnListFiles(ruleHolder.Load(), appLog)

	bgpWrapper := bgp.New(bgp.Config{
		ASN:                      cfg.ASN,
		RouterID:                 cfg.RouterID,
		LocalAddress:             cfg.LocalAddress,
		NextHop:                  cfg.NextHop,
		NextHopV6:                cfg.NextHopV6,
		PeerAddress:              cfg.Neighbor,
		PeerASN:                  cfg.NeighborASN,
		Passive:                  cfg.Passive,
		RemotePort:               cfg.RemotePort,
		HoldTime:                 time.Duration(cfg.HoldTimeSeconds) * time.Second,
		ConnectRetry:             time.Duration(cfg.ConnectRetrySeconds) * time.Second,
		Debug:                    *debug,
		IdleHold:                 time.Duration(cfg.IdleHoldSeconds) * time.Second,
		GracefulRestart:          cfg.GracefulRestart,
		GracefulRestartTime:      time.Duration(cfg.GracefulRestartTimeSeconds) * time.Second,
		LongLivedGracefulRestart: cfg.LLGREnabled,
		LongLivedStaleTime:       time.Duration(cfg.LLGRStaleSeconds) * time.Second,
	}, appLog)

	store := state.New(state.Config{
		MaxIPEntries:     cfg.MaxIPEntries,
		WheelGranularity: time.Duration(cfg.GranularitySeconds) * time.Second,
		WheelSlots:       3600,
		MinRouteTTL:      effectiveMinTTL(cfg),
		EventQueueSize:   cfg.EventQueueSize,
		Debug:            *debug,
		TrackFQDN:        cfg.TrackFQDN,
		KeepLease:        keepLease(cfg),
		KeepMaxHold:      time.Duration(cfg.KeepaliveMaxHoldSeconds) * time.Second,
	}, bgpWrapper, appLog)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := bgpWrapper.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "bgp.Start:", err)
		return 1
	}
	defer bgpWrapper.Stop()

	go store.Run(ctx)

	if cfg.KeepaliveEnabled {
		go runKeepalive(ctx, keepaliveSettings{
			Command:       cfg.ConntrackCmd,
			Args:          strings.Fields(cfg.ConntrackArgs),
			Interval:      time.Duration(cfg.KeepalivePollSeconds) * time.Second,
			TCPIdleLimit:  time.Duration(cfg.TCPIdleLimitSeconds) * time.Second,
			TCPEstTimeout: time.Duration(cfg.TCPEstTimeoutSeconds) * time.Second,
		}, store, execStream, appLog)
	}

	go watchCommunityRules(ctx, cfg.RulesFile, time.Duration(cfg.RulesReloadSeconds)*time.Second,
		rulesFP{main: rulesMainFP, refs: refsTag(loaded.refs)}, loadOptions, &ruleHolder, cfg.NeighborASN, appLog)

	go func() {
		ctl := &controlServer{cfg: cfg, store: store, rules: &ruleHolder, session: bgpWrapper.SessionInfo, now: time.Now, flushCfg: flushSettingsFromConfig(cfg)}
		if err := serveControl(ctx, ctl, appLog); err != nil {
			appLog.Warnf("main: control socket stopped: %v", err)
		}
	}()

	transportUp := make(chan struct{})
	if cfg.FlushOnStart {
		fs := flushSettingsFromConfig(cfg)
		// Only once the Python module's socket is accepting connections:
		// the re-resolved names must find somewhere to send their events.
		go func() {
			select {
			case <-transportUp:
			case <-ctx.Done():
				return
			}
			flushUnboundCache(ctx, fs, ruleHolder.Load(), appLog)
		}()
	}

	go func() {
		if err := serveTransport(cfg.ListenSocket, cfg.SocketMode, cfg.MaxInboundConns, store, &ruleHolder, appLog, func() { close(transportUp) }); err != nil {
			appLog.Warnf("main: transport stopped: %v", err)
		}
	}()

	appLog.Infof("main: unbound-route-state %s started (asn=%d, socket=%s, peer=%s, passive=%v)",
		version, cfg.ASN, cfg.ListenSocket, cfg.Neighbor, cfg.Passive)

	<-ctx.Done()
	appLog.Infof("main: shutting down")
	return 0
}
