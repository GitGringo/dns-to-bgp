// Package bgp wraps GoBGP, used purely as an embedded library (no gobgpd, no
// gobgp CLI), and implements state.Announcer: it speaks BGP to the peer
// (typically BIRD) and announces/withdraws /32 and /128 routes. The logic that
// decides *when* to announce or withdraw lives in internal/state.
//
// GoBGP notes: the peer is added with AddPeer, and GracefulRestart /
// LongLivedGracefulRestart are configured per AFI-SAFI on api.AfiSafi rather
// than as flat peer fields.
package bgp

import "time"

// Config mirrors the BGP-related part of the [general] section of state.conf:
// ASN, local/peer addresses, GR/LLGR. All fields map directly to a GoBGP
// api.Global/api.Peer construction in wrapper.go; nothing here is
// GoBGP-specific, so the config file format would not have to change if the
// underlying BGP library did.
type Config struct {
	ASN      uint32 // local ASN; equal to the peer's ASN for an iBGP session
	RouterID string // dotted-quad, e.g. the server's own address

	LocalAddress string // source address for the session
	PeerAddress  string // the peer's (BIRD's) address on this session
	PeerASN      uint32 // the peer's ASN; equals ASN for iBGP

	// ListenPortOverride, if >0, overrides the default listen port (1790
	// - see wrapper.go's Start doc comment for why not 179). 0 uses the
	// default. Only relevant when Passive is true.
	ListenPortOverride int

	// Passive controls session direction. It is deliberately explicit rather
	// than defaulted, because the two setups need opposite values:
	//
	//   Passive: true  - this side listens and BIRD dials out (the BIRD
	//   protocol must NOT be configured with "passive on").
	//
	//   Passive: false - this side dials out and BIRD listens (BIRD has
	//   "passive on"; with both sides passive nothing would ever connect).
	//   RemotePort must then be set to the port BIRD listens on.
	Passive bool

	// RemotePort is the port to dial when Passive is false. Only
	// meaningful for the active (dial-out) direction; ignored when
	// Passive is true (this side listens instead of connecting).
	RemotePort int

	HoldTime time.Duration // session hold time, e.g. 240s

	// Debug raises GoBGP's own log level to Debug (its default here is Warn),
	// which makes outgoing connection attempts and FSM transitions visible
	// ("failed to connect", ...). Wired to the daemon's -debug flag.
	Debug bool

	// ConnectRetry is how often an ACTIVE (non-passive) session re-dials the
	// peer while it is down. GoBGP's own default is 120s, which turned a BIRD
	// restart into minutes of "Idle": after the first immediate attempt is
	// refused, the next one only came ~90-120s later. Default here: 10s (a
	// refused TCP connect to a local BIRD is free). GoBGP enforces a 2s floor.
	ConnectRetry time.Duration

	// IdleHold is the pause before reconnecting after a session reset
	// (GoBGP default 30s). Default here: 5s. Must be >= 1s: GoBGP treats 0
	// as "unset" and applies its own 30s.
	IdleHold time.Duration

	GracefulRestart          bool
	GracefulRestartTime      time.Duration
	LongLivedGracefulRestart bool
	LongLivedStaleTime       time.Duration

	// NextHop is put on every IPv4 route's NEXT_HOP attribute - BIRD
	// overwrites it anyway via "next hop self" on its side of the session
	// (templates.conf.txt / upstreams.conf pattern), but BGP still
	// requires a syntactically valid value on the wire. Conventionally
	// this is just LocalAddress; kept separate in case that ever needs
	// to differ.
	NextHop string

	// NextHopV6 is the IPv6 equivalent - required separately because
	// RFC 4760 mandates the next-hop's address family match the NLRI's,
	// even when the session transport itself is IPv4 (confirmed by a
	// real BIRD rejection: "Invalid NEXT_HOP attribute - mismatched
	// address family" when this wrapper reused the IPv4 NextHop for an
	// IPv6 route). Empty means "no IPv6 default configured" - Announce
	// will fail loudly for an IPv6 route rather than send an
	// address-family-mismatched next-hop again.
	NextHopV6 string

	// AnnounceTimeout bounds every single AddPath/DeletePath call. These
	// are called synchronously from state.Store's single-owner goroutine
	// (state.Announcer's documented contract: "MUST NOT block").
	//
	// v4 note: unlike v3, GoBGP v4's AddPath/DeletePath (pkg/apiutil)
	// do not take a context.Context (see the official example at
	// github.com/osrg/gobgp/blob/master/docs/sources/lib.md - AddPath is
	// called with just the request struct). Without a cancellable ctx,
	// the timeout here is enforced by running the call in a goroutine and
	// racing it against time.After in wrapper.go - it stops the caller
	// from blocking past this duration, but cannot actually abort GoBGP's
	// own in-flight call if it's genuinely stuck (a background goroutine
	// is left to finish and log on its own). Default: 500ms.
	AnnounceTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.AnnounceTimeout <= 0 {
		c.AnnounceTimeout = 500 * time.Millisecond
	}
	if c.NextHop == "" {
		c.NextHop = c.LocalAddress
	}
	if c.ConnectRetry <= 0 {
		c.ConnectRetry = 10 * time.Second
	}
	if c.IdleHold < time.Second {
		c.IdleHold = 5 * time.Second
	}
	return c
}

// ListenPort returns the configured override, or 1790 by default - a
// non-privileged port, deliberately not 179 (see wrapper.go's Start doc
// comment). Matches the existing project convention of dedicated
// non-standard ports for internal-only BGP sessions (downstreams.conf.txt's
// "port 1179" for the messengers feed).
func (c Config) ListenPort() int {
	if c.ListenPortOverride > 0 {
		return c.ListenPortOverride
	}
	return 1790
}
