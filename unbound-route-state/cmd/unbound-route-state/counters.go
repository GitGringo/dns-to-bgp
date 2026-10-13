package main

import (
	"sync/atomic"
	"time"
)

// daemonCounters are the daemon-level (non-state.Store) counters shown by
// `ctl stats`. Plain atomics, only ever incremented with Add(1) on the
// paths that already exist - no locks, allocations or logging added to the
// event path. Process-lifetime only: reset on restart, by design.
//
// A package-level singleton (ctr) rather than a dependency threaded through
// every constructor: one daemon process, one set of counters.
type daemonCounters struct {
	startedAt time.Time

	// Ingest (transport.go). A "line" is one NDJSON event from the Python
	// module; a "record" is one A/AAAA address inside it.
	connsAccepted    atomic.Uint64
	connsRejected    atomic.Uint64
	linesReceived    atomic.Uint64 // valid lines
	linesMalformed   atomic.Uint64 // not JSON
	linesInvalid     atomic.Uint64 // JSON, but failed schema validation
	recordsSubmitted atomic.Uint64 // handed to Store.Submit (drops are counted by the Store)
	recordsInvalid   atomic.Uint64

	// Classification / rules (communities.go, rulewatch.go).
	unclassified      atomic.Uint64 // matched no rule and there is no [default:missed]
	rulesReloadOK     atomic.Uint64
	rulesReloadFailed atomic.Uint64
	rulesGeneration   atomic.Uint64 // 1 after the startup load, +1 per successful reload
	rulesLoadedAt     atomic.Int64  // unix seconds of the current generation

	// Unbound cache flush at startup (flush.go): per-command results.
	flushOK     atomic.Uint64
	flushFailed atomic.Uint64
	flushedAt   atomic.Int64 // unix seconds when the startup flush finished; 0 = it did not run

	// Traffic keepalive (keepalive.go).
	kaPollsOK     atomic.Uint64
	kaPollsFailed atomic.Uint64
	kaLastPollAt  atomic.Int64 // unix seconds; 0 = no poll yet
	kaLastMatched atomic.Int64 // cached IPs with a live flow in the last poll
	kaLastErr     atomic.Value // string; "" = last poll ok
}

var ctr = daemonCounters{startedAt: time.Now()}

func (c *daemonCounters) noteRulesLoaded() {
	c.rulesGeneration.Add(1)
	c.rulesLoadedAt.Store(time.Now().Unix())
}
