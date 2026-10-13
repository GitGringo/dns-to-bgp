package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"

	"unbound-route-state/internal/state"
)

// wireEvent is the NDJSON event sent by the Python module, one per line:
// {"fqdn": "example.com", "ttl": 1789, "type": "A", "records": ["192.0.2.76"]}
type wireEvent struct {
	FQDN    string   `json:"fqdn"`
	TTL     int64    `json:"ttl"`
	Type    string   `json:"type"`
	Records []string `json:"records"`
}

// maxLineBytes bounds one NDJSON line; a legitimate event with dozens of A
// records still fits comfortably below it.
const maxLineBytes = 8192

// transport owns the UNIX socket listener. Everything is bounded: a fixed
// number of connections, a fixed maximum line size and a non-blocking Submit,
// so there is no unbounded queue anywhere in this file.
type transport struct {
	store   *state.Store
	rules   *atomic.Pointer[ruleSet] // hot-reloadable, see rulewatch.go
	log     logger
	maxConn int32
	active  atomic.Int32
}

type logger interface {
	Warnf(format string, args ...any)
	Infof(format string, args ...any)
}

func serveTransport(path string, mode int, maxConns int, store *state.Store, rules *atomic.Pointer[ruleSet], log logger, onListening func()) error {
	_ = os.Remove(path) // stale socket from a previous run - best effort, ignore error
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("transport: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, os.FileMode(mode)); err != nil {
		return fmt.Errorf("transport: chmod %s: %w", path, err)
	}

	t := &transport{store: store, rules: rules, log: log, maxConn: int32(maxConns)}
	log.Infof("transport: listening on %s (mode %o, max %d connections)", path, mode, maxConns)
	if onListening != nil {
		onListening()
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("transport: accept: %w", err)
		}
		if t.active.Load() >= t.maxConn {
			ctr.connsRejected.Add(1)
			log.Warnf("transport: max_inbound_conns=%d reached, rejecting new connection", maxConns)
			conn.Close()
			continue
		}
		ctr.connsAccepted.Add(1)
		t.active.Add(1)
		go t.handle(conn)
	}
}

func (t *transport) handle(conn net.Conn) {
	defer func() {
		conn.Close()
		t.active.Add(-1)
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, maxLineBytes), maxLineBytes)

	for sc.Scan() {
		t.handleLine(sc.Bytes())
	}
	if err := sc.Err(); err != nil {
		// Includes bufio.ErrTooLong for an oversized line: drop the whole
		// connection rather than trying to resynchronise a corrupted NDJSON
		// stream.
		t.log.Warnf("transport: connection error, closing: %v", err)
	}
}

func (t *transport) handleLine(line []byte) {
	var ev wireEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		ctr.linesMalformed.Add(1)
		t.log.Warnf("transport: malformed JSON, dropped: %v", err)
		return
	}

	// Schema validation: reject malformed events and move on.
	if ev.FQDN == "" || ev.TTL < 0 || (ev.Type != "A" && ev.Type != "AAAA") || len(ev.Records) == 0 {
		ctr.linesInvalid.Add(1)
		t.log.Warnf("transport: invalid event for fqdn=%q type=%q, dropped", ev.FQDN, ev.Type)
		return
	}

	ctr.linesReceived.Add(1)
	fqdnHash := hashFQDN(ev.FQDN)
	rs := t.rules.Load() // one Load per event: rules and defaultRule from the same generation
	attrs := classify(ev.FQDN, rs.rules, rs.defaultRule)
	if !rs.countHits(ev.FQDN) {
		ctr.unclassified.Add(1)
	}
	if len(attrs.Communities) == 0 && len(attrs.LargeCommunities) == 0 {
		// A domain the Python module forwarded but that matches no rule here
		// means the two components' pattern lists disagree. Make that loud.
		t.log.Warnf("transport: fqdn=%q matched no community rule, route(s) will carry no community", ev.FQDN)
	}

	wantV4 := ev.Type == "A"
	for _, rec := range ev.Records {
		addr, err := netip.ParseAddr(strings.TrimSpace(rec))
		if err != nil || addr.Is4() != wantV4 {
			ctr.recordsInvalid.Add(1)
			t.log.Warnf("transport: invalid %s record %q for fqdn=%q, skipped", ev.Type, rec, ev.FQDN)
			continue
		}

		var ip state.IP
		if wantV4 {
			b := addr.As4()
			ip = state.IPv4ToIP(b[0], b[1], b[2], b[3])
		} else {
			ip = state.IPv6ToIP(addr.As16())
		}

		ctr.recordsSubmitted.Add(1)
		t.store.Submit(state.Event{
			IP:         ip,
			FQDN:       ev.FQDN,
			FQDNHash:   fqdnHash,
			TTLSeconds: uint32(ev.TTL),
			Attrs:      attrs,
		})
	}
}

// hashFQDN is FNV-1a - the same algorithm used consistently wherever a
// dedup hash of the fqdn is needed (internal/state deliberately never
// sees the string itself, only this hash - see internal/state/types.go's
// Event.FQDNHash doc comment for why that split exists).
func hashFQDN(fqdn string) uint64 {
	var h uint64 = 14695981039346656037
	fqdn = strings.ToLower(strings.TrimSuffix(fqdn, "."))
	for i := 0; i < len(fqdn); i++ {
		h ^= uint64(fqdn[i])
		h *= 1099511628211
	}
	return h
}
