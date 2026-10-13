// Package state implements the in-memory route state of the DNS-to-BGP
// daemon:
//
//   - IP deduplication across multiple FQDNs: one IP may be kept alive by
//     several domains at once and is withdrawn only when the last one expires.
//   - TTL-driven expiry via a single-level timing wheel instead of a per-IP
//     time.Timer, which keeps the memory footprint small (the daemon is meant
//     to stay within a few tens of MB).
//   - A single-owner ("actor") concurrency model: all state mutation happens
//     sequentially inside the goroutine started by Store.Run, reached only
//     through the event channel and the wheel's own tick. Renewal and expiry
//     are two message types in the same queue, so they can never race.
//
// The package deliberately knows nothing about fqdn strings (only their hash),
// the BGP wire protocol or persistence; those belong to cmd/unbound-route-state
// and internal/bgp.
package state

import (
	"fmt"
	"time"
)

// IP is a dual-stack address: IPv4 and IPv6 share one 16-byte representation.
// This favours a single simple code path over separate v4/v6 storage, at the
// cost of IPv4 entries also paying for 16 bytes.
//
// IPv4 addresses are stored left-padded with 12 zero bytes (bytes[0:12]
// == 0, bytes[12:16] == the four octets) - not the standard ::ffff:0:0/96
// mapped-IPv6 prefix, since this type never needs to interoperate with
// net.IP's own convention; it only needs to be internally consistent
// between IPv4ToIP/IsIPv4 and everything that constructs an IP. A native
// IPv6 address that happens to fall in the same numeric range (extremely
// unlikely for any real DNS-derived CDN/service address) would be
// misclassified as IPv4 by IsIPv4 - an accepted edge case.
type IP [16]byte

// IPv4ToIP builds an IP from four IPv4 octets.
func IPv4ToIP(a, b, c, d byte) IP {
	var ip IP
	ip[12], ip[13], ip[14], ip[15] = a, b, c, d
	return ip
}

// IPv6ToIP builds an IP from sixteen raw IPv6 bytes.
func IPv6ToIP(b [16]byte) IP { return IP(b) }

// IsIPv4 reports whether ip was built by IPv4ToIP (bytes[0:12] all
// zero) - see the type doc comment for the accepted edge case.
func (ip IP) IsIPv4() bool {
	for i := 0; i < 12; i++ {
		if ip[i] != 0 {
			return false
		}
	}
	return true
}

// String renders the address for logging - dotted-quad for IPv4,
// colon-hex for IPv6. Not optimized (fmt.Sprintf), only used on
// log/error paths, never in the hot event path.
func (ip IP) String() string {
	if ip.IsIPv4() {
		return fmt.Sprintf("%d.%d.%d.%d", ip[12], ip[13], ip[14], ip[15])
	}
	return fmt.Sprintf("%x:%x:%x:%x:%x:%x:%x:%x",
		uint16(ip[0])<<8|uint16(ip[1]), uint16(ip[2])<<8|uint16(ip[3]),
		uint16(ip[4])<<8|uint16(ip[5]), uint16(ip[6])<<8|uint16(ip[7]),
		uint16(ip[8])<<8|uint16(ip[9]), uint16(ip[10])<<8|uint16(ip[11]),
		uint16(ip[12])<<8|uint16(ip[13]), uint16(ip[14])<<8|uint16(ip[15]))
}

// Community encodes a single standard BGP (ASN, value) community pair —
// RFC 1997, 16+16 bits in practice - as one uint64 instead of a pair of
// uint32 to avoid per-field alignment padding in the ipState.attrs.Communities
// slice (memory budget).
type Community uint64

// NewCommunity packs an (ASN, value) pair, e.g. NewCommunity(64512, 100)
// for the community 64512:100.
func NewCommunity(asn, value uint32) Community {
	return Community(uint64(asn)<<32 | uint64(value))
}

func (c Community) ASN() uint32   { return uint32(c >> 32) }
func (c Community) Value() uint32 { return uint32(c) }

// LargeCommunity is an RFC 8092 (ASN, LocalData1, LocalData2) triple, a second,
// wider tagging mechanism next to the standard Community (both accumulate
// across matching rule blocks; see classify() in cmd/unbound-route-state).
type LargeCommunity struct {
	ASN, Data1, Data2 uint32
}

// RouteAttrs bundles every BGP attribute a route can carry beyond its
// NLRI. A domain can match several communities.conf blocks at once -
// Communities and LargeCommunities accumulate (union) across every
// matching set, while NextHop/ASPath/LocalPref/MED are taken from the
// FIRST matching set only. That merge happens upstream, in
// cmd/unbound-route-state's classify() - this package only stores and
// forwards the already-merged result; it has no opinion on how a
// RouteAttrs value was built.
//
// LocalPref/MED use explicit HasX flags rather than treating 0 as "unset"
// - 0 is a legal, if unusual, value for either attribute, and silently
// conflating "unset" with "explicitly zero" would be a real (if obscure)
// correctness bug, not just a style choice.
type RouteAttrs struct {
	Communities      []Community
	LargeCommunities []LargeCommunity
	NextHop          string // IPv4 routes only; empty = caller (internal/bgp) uses its own default
	NextHopV6        string // IPv6 routes only; empty = default (see internal/bgp pickNextHop for the legacy rule)
	ASPath           []uint32
	LocalPref        uint32
	HasLocalPref     bool
	MED              uint32
	HasMED           bool
}

// Event is a single dedup-relevant fact extracted from one DNS answer.
// It is the boundary type between this package and internal/transport +
// cmd/unbound-route-state's community classifier: by the time an Event
// reaches Store.Submit, fqdn wildcard-matching and attribute
// classification have already happened upstream (this package never inspects
// fqdn strings).
type Event struct {
	IP IP

	// FQDN is only used for debug logging and the optional FQDN tracking
	// (Config.Debug / Config.TrackFQDN). It is never retained by
	// ipState/domainBinding; see Store's debugDomains for the one place where
	// fqdn strings may be kept, and only when one of those options is on.
	FQDN string

	// FQDNHash is a 64-bit hash (FNV-1a, xxhash, or similar - the exact
	// algorithm is the caller's choice, but it MUST be the same algorithm
	// used consistently by every producer of Events, otherwise dedup
	// across restarts/producers silently breaks) of the owner fqdn that
	// matched a wildcard pattern. Used only as a dedup key; the actual
	// string is never retained by this package.
	FQDNHash uint64

	// TTLSeconds is the DNS TTL as received, verbatim. 0 is a valid value
	// ("do not cache") and needs no special case: it flows through the
	// normal max()-over-domains logic.
	TTLSeconds uint32

	// Attrs is only consulted when this event causes a *new* IP to be
	// created - see the "known limitation" note on the Store type for
	// what happens (nothing) when a later domain on an already-announced
	// IP would have contributed additional attrs.
	Attrs RouteAttrs
}

// domainBinding is one (domain, expiry) fact contributing to an IP's
// lifetime. The fqdn itself is intentionally not retained; it is needed only
// once, at classification time, upstream of this package.
type domainBinding struct {
	fqdnHash  uint64
	expiresAt uint32 // unix seconds, not time.Time (4 bytes instead of 24)
}

// ipState is the per-IP record, the core unit of the memory budget. domains is
// a slice, not a map: 1-3 entries per IP is by far the most common case (several
// hostnames of one CDN sharing an address), where a linear scan beats a map's
// fixed per-entry overhead.
type ipState struct {
	ip        IP
	domains   []domainBinding
	attrs     RouteAttrs // frozen at first-announce time - see Store doc
	announced bool

	// Observability only (ctl routes/route): unix seconds of the first
	// announce, and how many later events hit this IP while it stayed in
	// the cache (TTL renewals and new-domain joins alike). Never used for
	// any decision.
	announcedAt uint32
	renewals    uint32

	// Traffic keepalive (NoteTraffic): lastTraffic = unix seconds a live flow
	// to this IP was last observed; holdUntil = the route is kept at least
	// until then even if every DNS TTL has lapsed; holdSince = start of the
	// current keepalive streak (the max-hold ceiling counts from it; a fresh
	// DNS event for the IP resets it to 0).
	lastTraffic uint32
	holdUntil   uint32
	holdSince   uint32

	// Timing-wheel intrusive linked-list membership (pointer-based).
	wheelNext  *ipState
	wheelPrev  *ipState
	slot       int  // index into wheel.slots; -1 if not in a slot
	inOverflow bool // true if linked into wheel.overflow instead of a slot
}

// Announcer is implemented by the BGP integration layer (internal/bgp). It is
// invoked synchronously and MUST NOT block: both methods are called directly
// from Store's single owner goroutine, and GoBGP's AddPath/DeletePath are
// in-memory RIB operations that return quickly. An implementation that needs
// to do something slower must make it asynchronous internally rather than push
// the latency onto this package's event loop.
type Announcer interface {
	Announce(ip IP, attrs RouteAttrs)
	Withdraw(ip IP, attrs RouteAttrs)
}

// Logger is a minimal, dependency-free logging seam so this package does
// not pin the rest of the daemon to a specific logging library. Rate limiting
// of repeated messages is the concrete Logger implementation's responsibility:
// Store calls Warnf/Infof on every occurrence and trusts the implementation
// to aggregate.
type Logger interface {
	Warnf(format string, args ...any)
	Infof(format string, args ...any)
}

// Config holds the tunables from the [limits], [ttl] and [keepalive] sections
// of state.conf.
type Config struct {
	// MaxIPEntries is the defensive cap on cached IPs (suggested: 50000).
	// 0 means "no limit", which is not recommended.
	MaxIPEntries int

	// WheelGranularity and WheelSlots define the timing wheel's tick size
	// and horizon (horizon = WheelGranularity * WheelSlots).
	// Default: 1s / 3600 slots (1 hour horizon); a coarser granularity
	// (5-10s) suits small embedded systems. Entries with a TTL beyond the
	// horizon are held in an overflow list and migrated in as they come
	// into range - see wheel.go.
	WheelGranularity time.Duration
	WheelSlots       int

	// MinRouteTTL is an optional floor for the route lifetime (default 0:
	// disabled, strict TTL fidelity). When >0 it only affects the internal
	// expiry calculation, never anything announced over BGP (BGP itself
	// carries no TTL attribute).
	MinRouteTTL time.Duration

	// AnnounceCoalesce is accepted for configuration compatibility but is
	// not applied: the wheel's O(1) reschedule makes coalescing unnecessary.
	AnnounceCoalesce time.Duration

	// EventQueueSize is the capacity of the bounded channel behind Submit;
	// events are dropped when it is full. Default: 1024.
	EventQueueSize int

	// Debug enables ANNOUNCE/WITHDRAW trace logging (domain + TTL on
	// announce, domain on withdraw). Off by default - when on, Store
	// allocates a small side-table (debugDomains) purely to remember
	// which fqdn(s) an IP came from until it's withdrawn; ipState/
	// domainBinding themselves never grow because of this (see store.go).
	Debug bool

	// TrackFQDN keeps, per live IP, the fqdn string(s) that hold it alive,
	// so snapshots (ctl routes) can show them. Same side-table mechanism as
	// Debug (see store.go debugDomains), same guarantee: ipState/
	// domainBinding never grow because of it. Costs roughly one short
	// string per (IP, domain) pair; negligible against MaxIPEntries.
	TrackFQDN bool

	// KeepLease > 0 turns the traffic keepalive on: after NoteTraffic reports a
	// live flow to an IP, the route is kept for KeepLease past the last
	// observation, however short its DNS TTL was. KeepMaxHold (0 = unlimited)
	// caps one keepalive streak (counted from its first observation, restarted
	// by a new DNS event for the IP) so a stuck flow cannot pin a route forever.
	KeepLease   time.Duration
	KeepMaxHold time.Duration

	// OnWithdraw, if non-nil, is called from Store's owner goroutine for
	// every route that leaves the cache (all domains lapsed), with the
	// final RouteInfo. It MUST NOT block. This is the extension point for
	// a future "recently withdrawn routes" history (a bounded ring buffer
	// fed from here) - intentionally unused today.
	OnWithdraw func(RouteInfo)
}

// RouteInfo is a point-in-time, self-contained copy of one cached route,
// safe to hand to other goroutines (Store.Snapshot, OnWithdraw). Times are
// unix seconds. The same type is deliberately used for live snapshots and
// for the withdraw hook, so a future history store needs no new schema.
type RouteInfo struct {
	IP           IP
	FQDNs        []string // empty unless Config.TrackFQDN (or Debug) is on
	Attrs        RouteAttrs
	DomainCount  int
	AnnouncedAt  uint32
	DNSExpiresAt uint32 // latest expiry over all domains (0 once they have all lapsed)
	ExpiresAt    uint32 // when it gets withdrawn: DNSExpiresAt, or HoldUntil if later
	Renewals     uint32
	LastTraffic  uint32 // 0 = never seen (or keepalive off)
	HoldUntil    uint32 // 0 = not held by traffic
}

// Traffic is one observation for NoteTraffic: a live flow to IP existed at
// LastSeen (unix seconds).
type Traffic struct {
	IP       IP
	LastSeen uint32
}
