package state

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Store owns all IP/domain state and is its single writer (actor model):
// every mutation - a new or renewed DNS event, or a wheel tick discovering an
// expiry - is handled sequentially inside Run's goroutine. This makes a race
// between renewal and expiry structurally impossible: both are just messages
// arriving at the same select loop.
//
// Submit (and the read-only query methods built on channels) are the only
// methods safe to call from other goroutines. Every other interaction,
// including reading fields of ipState, must happen from inside Run.
type Store struct {
	cfg       Config
	announcer Announcer
	log       Logger

	events chan Event // bounded ingress queue

	// Rate-limited logging for Submit()'s drop path. Without it, a burst that
	// overflows the queue would drop events with no trace in the log, only the
	// DroppedEvents counter. Submit() can be called concurrently from several
	// reader goroutines (max_inbound_conns > 1), hence the mutex; it only
	// matters while actively dropping and is not a hot-path cost.
	dropLogMu    sync.Mutex
	dropLogLast  time.Time
	dropLogCount int64

	byIP map[IP]*ipState

	// debugDomains: IP -> fqdn(s) currently keeping it alive. Only
	// allocated when cfg.Debug (nil otherwise - reads on a nil map are
	// safe no-ops in Go, no need to guard every access). Deliberately
	// kept OUTSIDE ipState/domainBinding so those structs are byte-for-
	// byte identical in size whether or not Debug is ever used - see
	// Config.Debug's doc comment.
	debugDomains map[IP][]string
	wheel        *wheel

	nowFn func() uint32 // injectable for deterministic tests; real time by default

	snapshots chan snapshotReq // served by Run's goroutine - see Snapshot
	ipLists   chan chan []IP   // ListIPs requests
	traffic   chan trafficReq  // NoteTraffic requests

	stats Stats
}

type trafficReq struct {
	items []Traffic
	reply chan int
}

type snapshotReq struct {
	filter func(RouteInfo) bool // nil = everything; runs in Run's goroutine, must be cheap
	reply  chan []RouteInfo
}

// Stats are plain atomic counters - safe to read via StatsSnapshot from
// any goroutine (e.g. a future --status CLI or metrics endpoint) even
// though they are only ever written from Run's goroutine.
type Stats struct {
	ActiveIPs     atomic.Int64
	DroppedEvents atomic.Int64
	RejectedAtCap atomic.Int64
	Announces     atomic.Int64
	Withdraws     atomic.Int64

	// Renewals: event hit an existing IP and refreshed an existing domain's
	// TTL. DomainJoins: event hit an existing IP with a not-yet-seen domain.
	// EventsHandled: every event taken off the queue (new + renewal + join
	// + rejected-at-cap). TTLFloorApplied: MinRouteTTL raised an expiry.
	// QueueHighWatermark: deepest the ingress queue has been.
	Renewals           atomic.Int64
	TrafficExtended    atomic.Int64 // times a route's lifetime was pushed out by observed traffic
	DomainJoins        atomic.Int64
	EventsHandled      atomic.Int64
	TTLFloorApplied    atomic.Int64
	QueueHighWatermark atomic.Int64
}

// StatsView is a plain, non-atomic snapshot for callers that just want to
// read the numbers once (logging, a status command, etc.).
type StatsView struct {
	ActiveIPs     int64
	DroppedEvents int64
	RejectedAtCap int64
	Announces     int64
	Withdraws     int64

	Renewals           int64
	TrafficExtended    int64
	DomainJoins        int64
	EventsHandled      int64
	TTLFloorApplied    int64
	QueueHighWatermark int64
	QueueDepth         int64 // instantaneous
	QueueCapacity      int64
}

// New constructs a Store. It does not start Run - callers must do that in
// a goroutine of their own choosing (typically main.go).
func New(cfg Config, announcer Announcer, log Logger) *Store {
	if cfg.EventQueueSize <= 0 {
		cfg.EventQueueSize = 1024
	}
	if cfg.WheelSlots <= 0 {
		cfg.WheelSlots = 3600
	}
	if cfg.WheelGranularity <= 0 {
		cfg.WheelGranularity = time.Second
	}

	s := &Store{
		cfg:       cfg,
		announcer: announcer,
		log:       log,
		events:    make(chan Event, cfg.EventQueueSize),
		byIP:      make(map[IP]*ipState),
		nowFn:     defaultNow,

		snapshots: make(chan snapshotReq),
		ipLists:   make(chan chan []IP),
		traffic:   make(chan trafficReq),
	}
	s.wheel = newWheel(cfg.WheelGranularity, cfg.WheelSlots, s.nowFn())
	if cfg.Debug || cfg.TrackFQDN {
		s.debugDomains = make(map[IP][]string)
	}
	return s
}

func defaultNow() uint32 { return uint32(time.Now().Unix()) }

// Submit is the non-blocking ingress point: the socket reader goroutine parses
// one NDJSON line, validates it, and calls Submit. It never retries and never
// blocks on a full queue; dropping is the correct behaviour here, because the
// reader must stay fast so it does not become the bottleneck upstream.
//
// Returns false if the event was dropped because the queue was full.
func (s *Store) Submit(ev Event) bool {
	select {
	case s.events <- ev:
		s.noteDepth()
		return true
	default:
		s.stats.DroppedEvents.Add(1)
		s.noteQueueDrop()
		return false
	}
}

// noteDepth records the queue's high-water mark: one len() and one atomic
// load on the success path, a CAS only when a new maximum is reached.
func (s *Store) noteDepth() {
	d := int64(len(s.events))
	for {
		hw := s.stats.QueueHighWatermark.Load()
		if d <= hw || s.stats.QueueHighWatermark.CompareAndSwap(hw, d) {
			return
		}
	}
}

// noteQueueDrop logs Submit()'s drop path at most once per 10s,
// aggregating the count in between - same discipline as the Python
// side's transport.py _note_drop, and as RejectedAtCap's existing
// Warnf below, just previously missing here specifically.
func (s *Store) noteQueueDrop() {
	s.dropLogMu.Lock()
	defer s.dropLogMu.Unlock()
	s.dropLogCount++
	if !s.dropLogLast.IsZero() && time.Since(s.dropLogLast) < 10*time.Second {
		return
	}
	s.log.Warnf("dns-bgp state: events channel full (event_queue_size=%d), dropped %d event(s) since last log - raise event_queue_size if this recurs under normal (non-burst) load",
		s.cfg.EventQueueSize, s.dropLogCount)
	s.dropLogCount = 0
	s.dropLogLast = time.Now()
}

// Run is the Store's single owner goroutine and must be started
// exactly once. It returns when ctx is cancelled.
func (s *Store) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.WheelGranularity)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-s.events:
			s.handleEvent(ev)
		case req := <-s.snapshots:
			req.reply <- s.collect(req.filter)
		case reply := <-s.ipLists:
			reply <- s.listIPs()
		case req := <-s.traffic:
			req.reply <- s.applyTraffic(req.items)
		case <-ticker.C:
			s.handleTick()
		}
	}
}

// StatsSnapshot returns a point-in-time copy of the counters. Safe to
// call from any goroutine.
func (s *Store) StatsSnapshot() StatsView {
	return StatsView{
		ActiveIPs:     s.stats.ActiveIPs.Load(),
		DroppedEvents: s.stats.DroppedEvents.Load(),
		RejectedAtCap: s.stats.RejectedAtCap.Load(),
		Announces:     s.stats.Announces.Load(),
		Withdraws:     s.stats.Withdraws.Load(),

		Renewals:           s.stats.Renewals.Load(),
		TrafficExtended:    s.stats.TrafficExtended.Load(),
		DomainJoins:        s.stats.DomainJoins.Load(),
		EventsHandled:      s.stats.EventsHandled.Load(),
		TTLFloorApplied:    s.stats.TTLFloorApplied.Load(),
		QueueHighWatermark: s.stats.QueueHighWatermark.Load(),
		QueueDepth:         int64(len(s.events)),
		QueueCapacity:      int64(cap(s.events)),
	}
}

// Snapshot returns a consistent copy of the live route table. The copy is
// taken inside Run's goroutine (the table's only owner), so no locking is
// added to the event path; the cost is one pass over byIP while events
// queue up briefly. filter (optional) runs inside that pass - keep it
// cheap - so large tables aren't copied just to be thrown away. Returns
// ctx's error if Run doesn't serve the request in time (or isn't running).
func (s *Store) Snapshot(ctx context.Context, filter func(RouteInfo) bool) ([]RouteInfo, error) {
	req := snapshotReq{filter: filter, reply: make(chan []RouteInfo, 1)}
	select {
	case s.snapshots <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-req.reply:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// routeInfo copies one ipState into a RouteInfo. Run's goroutine only.
func (s *Store) routeInfo(st *ipState) RouteInfo {
	ri := RouteInfo{
		IP:           st.ip,
		Attrs:        st.attrs, // slices are never mutated after first announce, sharing is safe
		DomainCount:  len(st.domains),
		AnnouncedAt:  st.announcedAt,
		DNSExpiresAt: maxDomainExpiry(st),
		ExpiresAt:    maxExpiry(st),
		Renewals:     st.renewals,
		LastTraffic:  st.lastTraffic,
		HoldUntil:    st.holdUntil,
	}
	if s.debugDomains != nil {
		ri.FQDNs = append([]string(nil), s.debugDomains[st.ip]...)
	}
	return ri
}

func (s *Store) collect(filter func(RouteInfo) bool) []RouteInfo {
	out := make([]RouteInfo, 0, len(s.byIP))
	for _, st := range s.byIP {
		ri := s.routeInfo(st)
		if filter == nil || filter(ri) {
			out = append(out, ri)
		}
	}
	return out
}

// handleEvent implements the dedup logic:
//
//   - new IP                       → create, announce once, schedule.
//   - existing IP, new fqdnHash    → add domain, extend schedule if
//     needed, do NOT re-announce.
//   - existing IP, same fqdnHash   → renewal: update expiry, extend
//     schedule if needed, do NOT re-announce.
//
// Known limitation: an already-announced IP is never re-announced. If a
// second domain with different attributes later joins the same IP, those
// attributes are NOT applied retroactively; the route keeps the RouteAttrs it
// had at first announce (ipState.attrs). This is uncommon for CDN edge nodes
// but possible; revisit if a deployment shows one IP serving genuinely
// different attribute sets over time.
func (s *Store) handleEvent(ev Event) {
	s.stats.EventsHandled.Add(1)
	expiresAt := s.effectiveExpiry(ev.TTLSeconds)

	st, exists := s.byIP[ev.IP]
	if !exists {
		if s.cfg.MaxIPEntries > 0 && len(s.byIP) >= s.cfg.MaxIPEntries {
			s.stats.RejectedAtCap.Add(1)
			s.log.Warnf("dns-bgp state: max_ip_entries=%d reached, new IP dropped (no eviction)", s.cfg.MaxIPEntries)
			return
		}

		st = &ipState{
			ip:      ev.IP,
			domains: []domainBinding{{fqdnHash: ev.FQDNHash, expiresAt: expiresAt}},
			attrs:   ev.Attrs,
			slot:    -1,

			announcedAt: s.nowFn(),
		}
		s.byIP[ev.IP] = st
		s.wheel.insert(st, expiresAt)

		s.announcer.Announce(st.ip, st.attrs)
		s.stats.Announces.Add(1)
		s.stats.ActiveIPs.Add(1)
		if s.debugDomains != nil {
			s.debugDomains[ev.IP] = []string{ev.FQDN}
		}
		if s.cfg.Debug {
			s.log.Infof("ANNOUNCE ip=%s domain=%s ttl=%ds", st.ip, ev.FQDN, ev.TTLSeconds)
		}
		return
	}

	st.renewals++
	st.holdSince = 0 // DNS just reconfirmed this IP: the max-hold ceiling restarts

	updated := false
	for i := range st.domains {
		if st.domains[i].fqdnHash == ev.FQDNHash {
			st.domains[i].expiresAt = expiresAt
			updated = true
			break
		}
	}
	if updated {
		s.stats.Renewals.Add(1)
	} else {
		s.stats.DomainJoins.Add(1)
		st.domains = append(st.domains, domainBinding{fqdnHash: ev.FQDNHash, expiresAt: expiresAt})
		if s.debugDomains != nil && !slices.Contains(s.debugDomains[ev.IP], ev.FQDN) {
			s.debugDomains[ev.IP] = append(s.debugDomains[ev.IP], ev.FQDN)
		}
	}

	s.wheel.remove(st)
	s.wheel.insert(st, maxExpiry(st))
}

// effectiveExpiry applies the optional MinRouteTTL floor (disabled by default).
// The floor only ever affects the internal wheel schedule, never anything
// actually announced over BGP.
func (s *Store) effectiveExpiry(ttlSeconds uint32) uint32 {
	eff := ttlSeconds
	if s.cfg.MinRouteTTL > 0 {
		floor := uint32(s.cfg.MinRouteTTL / time.Second)
		if eff < floor {
			eff = floor
			s.stats.TTLFloorApplied.Add(1)
		}
	}
	return s.nowFn() + eff
}

// handleTick advances the wheel to catch up with real elapsed time —
// draining whatever falls due along the way - and resolves each entry:
// withdraw if every domain has now actually lapsed, or reschedule if a
// renewal moved the true expiry later after this entry was originally
// scheduled (the slot is re-set to the new nearest expiry).
//
// This deliberately does NOT assume exactly one granularity period has
// elapsed since the previous call, even though Run drives it from a
// single time.Ticker tick. Go's time.Ticker documents that it drops
// ticks instead of queueing them when the receiver falls behind - so
// under sustained event load (the single-owner goroutine spending most
// of its time on the `case ev := <-s.events` branch) the wheel's own
// internal clock could otherwise lag further and further behind real
// time with no way to ever catch up on its own. Looping here until the
// wheel's nowSlotTime reaches s.nowFn() closes that gap on every call
// instead of letting it accumulate. The loop is capped at one full wheel
// revolution - beyond that every slot is already known to be empty, so a
// very long stall (e.g. embedded suspend/resume) is resolved with a
// single fastForward instead of spinning through empty slots.
func (s *Store) handleTick() {
	now := s.nowFn()

	steps, maxSteps := 0, len(s.wheel.slots)
	for s.wheel.nowSlotTime+s.wheel.granularitySeconds <= now && steps < maxSteps {
		for _, st := range s.wheel.tick() {
			s.processExpiry(st, now)
		}
		steps++
	}
	if s.wheel.nowSlotTime+s.wheel.granularitySeconds <= now {
		s.wheel.fastForward(now)
	}
}

func (s *Store) processExpiry(st *ipState, now uint32) {
	kept := st.domains[:0]
	for _, d := range st.domains {
		if d.expiresAt > now {
			kept = append(kept, d)
		}
	}
	st.domains = kept

	if len(st.domains) == 0 && st.holdUntil <= now {
		delete(s.byIP, st.ip)
		s.announcer.Withdraw(st.ip, st.attrs)
		s.stats.Withdraws.Add(1)
		s.stats.ActiveIPs.Add(-1)
		if s.cfg.OnWithdraw != nil {
			// st.domains is already empty here, so report `now` as the end of life.
			ri := s.routeInfo(st)
			ri.ExpiresAt = now
			s.cfg.OnWithdraw(ri)
		}
		if s.cfg.Debug {
			s.log.Infof("WITHDRAW ip=%s domain=%s", st.ip, strings.Join(s.debugDomains[st.ip], ","))
		}
		if s.debugDomains != nil {
			delete(s.debugDomains, st.ip)
		}
		return
	}

	s.wheel.insert(st, maxExpiry(st))
}

// ListIPs returns the addresses currently in the cache (no per-route copy:
// this is what the traffic poller needs every few seconds). Like Snapshot, it
// is served by Run's goroutine, so the table needs no lock.
func (s *Store) ListIPs(ctx context.Context) ([]IP, error) {
	reply := make(chan []IP, 1)
	select {
	case s.ipLists <- reply:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-reply:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Store) listIPs() []IP {
	out := make([]IP, 0, len(s.byIP))
	for ip := range s.byIP {
		out = append(out, ip)
	}
	return out
}

// NoteTraffic reports that live flows to these IPs were observed (the
// keepalive's input). It returns how many routes had their lifetime pushed
// out. Unknown IPs (withdrawn meanwhile) are ignored. With KeepLease == 0 the
// observation is only recorded (LastTraffic), nothing is extended.
func (s *Store) NoteTraffic(ctx context.Context, items []Traffic) (int, error) {
	req := trafficReq{items: items, reply: make(chan int, 1)}
	select {
	case s.traffic <- req:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	select {
	case n := <-req.reply:
		return n, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (s *Store) applyTraffic(items []Traffic) (extended int) {
	now := s.nowFn()
	lease := uint32(s.cfg.KeepLease / time.Second)
	maxHold := uint32(s.cfg.KeepMaxHold / time.Second)
	for _, it := range items {
		st := s.byIP[it.IP]
		if st == nil {
			continue
		}
		if it.LastSeen > st.lastTraffic {
			st.lastTraffic = it.LastSeen
		}
		if lease == 0 {
			continue
		}
		if st.holdSince == 0 {
			st.holdSince = now
		}
		until := it.LastSeen + lease
		if maxHold > 0 {
			if ceiling := st.holdSince + maxHold; until > ceiling {
				until = ceiling
			}
		}
		if until <= st.holdUntil {
			continue
		}
		before := maxExpiry(st)
		st.holdUntil = until
		if after := maxExpiry(st); after > before {
			s.wheel.remove(st)
			s.wheel.insert(st, after)
			s.stats.TrafficExtended.Add(1)
			extended++
		}
	}
	return extended
}
