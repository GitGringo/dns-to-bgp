package bgp

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	native "github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"

	"unbound-route-state/internal/state"
)

// Wrapper embeds a GoBGP v4 server (library mode only, no gobgpd and no gobgp
// CLI) and implements state.Announcer.
//
// Three GoBGP details worth knowing: DeletePath returns a single error;
// apiutil.Path.Family is the native bgp.Family type (not api.Family); and
// DeletePath needs the same next-hop attribute as the original announcement to
// match an existing path ("nexthop not found" otherwise).
type Wrapper struct {
	cfg Config
	log state.Logger
	srv *server.BgpServer
}

func New(cfg Config, log state.Logger) *Wrapper {
	return &Wrapper{cfg: cfg.withDefaults(), log: log}
}

// Start brings the embedded BGP server up and configures the single
// session to the peer: a single TCP session carrying both AFI_IP and AFI_IP6
// unicast (one multiprotocol session, not two). The transport address family
// is independent of which NLRI families ride over it.
//
// Session direction is controlled by cfg.Passive - see its doc comment
// in config.go. When passive, this side listens (port from
// cfg.ListenPort(), deliberately not 179 - see below). When active,
// GoBGP is told not to listen at all (ListenPort: -1); it only dials out
// to cfg.PeerAddress:cfg.RemotePort - and cfg.RemotePort must be BIRD's
// *actual* listen port. Confirmed empirically: a passive BIRD protocol
// always listens on the global standard port 179 for every session
// regardless of any "port" given in its own neighbor clause - that
// "port" only takes effect when BIRD itself is the active/dialing side.
// Set RemotePort accordingly (179 when BIRD is passive, whatever custom
// port when BIRD is active and this daemon is passive/listening).
func (w *Wrapper) Start(ctx context.Context) error {
	lvl := &slog.LevelVar{}
	lvl.Set(slog.LevelWarn) // GoBGP's own internal logging, separate from state.Logger
	if w.cfg.Debug {
		lvl.Set(slog.LevelDebug) // shows dial attempts: "try to connect" / "failed to connect"
	}
	w.srv = server.NewBgpServer(server.LoggerOption(slog.Default(), lvl))
	go w.srv.Serve()

	global := &api.Global{
		Asn:      w.cfg.ASN,
		RouterId: w.cfg.RouterID,
	}
	if w.cfg.Passive {
		global.ListenPort = int32(w.cfg.ListenPort())
		global.ListenAddresses = []string{w.cfg.LocalAddress}
	} else {
		global.ListenPort = -1 // no incoming sessions accepted at all - we only dial out
	}

	if err := w.srv.StartBgp(ctx, &api.StartBgpRequest{Global: global}); err != nil {
		return fmt.Errorf("bgp: StartBgp: %w", err)
	}

	if err := w.srv.AddPeer(ctx, &api.AddPeerRequest{Peer: w.buildPeer()}); err != nil {
		return fmt.Errorf("bgp: AddPeer(%s): %w", w.cfg.PeerAddress, err)
	}
	return nil
}

// Stop shuts the embedded server down. Safe to call even if Start failed
// partway through.
func (w *Wrapper) Stop() {
	if w.srv != nil {
		w.srv.Stop()
	}
}

// buildPeer - config structs (PeerConf/GracefulRestart/AfiSafi/Timers)
// unchanged in shape from v3. Two AfiSafi entries now (IPv4 + IPv6
// unicast) on the same peer/session for dual-stack support.
func (w *Wrapper) buildPeer() *api.Peer {
	afiSafis := []*api.AfiSafi{
		w.afiSafiConfig(api.Family_AFI_IP),
		w.afiSafiConfig(api.Family_AFI_IP6),
	}

	transport := &api.Transport{
		LocalAddress:  w.cfg.LocalAddress,
		PassiveMode:   w.cfg.Passive,
		RemoteAddress: w.cfg.PeerAddress,
	}
	if !w.cfg.Passive && w.cfg.RemotePort > 0 {
		transport.RemotePort = uint32(w.cfg.RemotePort)
	}

	peer := &api.Peer{
		Conf: &api.PeerConf{
			NeighborAddress: w.cfg.PeerAddress,
			PeerAsn:         w.cfg.PeerASN,
			LocalAsn:        w.cfg.ASN,
			Description:     "unbound-route-state (DNS-to-BGP daemon)",
		},
		Transport: transport,
		Timers: &api.Timers{
			Config: &api.TimersConfig{
				HoldTime:               uint64(w.cfg.HoldTime / time.Second),
				KeepaliveInterval:      uint64(w.cfg.HoldTime / time.Second / 3),
				ConnectRetry:           uint64(w.cfg.ConnectRetry / time.Second),
				IdleHoldTimeAfterReset: uint64(w.cfg.IdleHold / time.Second),
			},
		},
		AfiSafis: afiSafis,
	}

	if w.cfg.GracefulRestart {
		peer.GracefulRestart = &api.GracefulRestart{
			Enabled:             true,
			NotificationEnabled: true,
			LonglivedEnabled:    w.cfg.LongLivedGracefulRestart,
			RestartTime:         uint32(w.cfg.GracefulRestartTime / time.Second),
		}
	}
	return peer
}

func (w *Wrapper) afiSafiConfig(afi api.Family_Afi) *api.AfiSafi {
	a := &api.AfiSafi{
		Config: &api.AfiSafiConfig{
			Family:  &api.Family{Afi: afi, Safi: api.Family_SAFI_UNICAST},
			Enabled: true,
		},
	}
	if w.cfg.LongLivedGracefulRestart {
		a.LongLivedGracefulRestart = &api.LongLivedGracefulRestart{
			Config: &api.LongLivedGracefulRestartConfig{
				Enabled:     true,
				RestartTime: uint32(w.cfg.LongLivedStaleTime / time.Second),
			},
		}
	}
	return a
}

// Announce implements state.Announcer. Called synchronously from
// state.Store's single-owner goroutine - must not block (see the
// interface's doc comment in internal/state/types.go).
func (w *Wrapper) Announce(ip state.IP, attrs state.RouteAttrs) {
	nlri, family, err := w.buildNLRI(ip)
	if err != nil {
		w.log.Warnf("bgp: building NLRI for %s failed: %v", ip, err)
		return
	}
	pathAttrs, err := w.buildAttrs(ip, attrs)
	if err != nil {
		w.log.Warnf("bgp: building attributes for %s failed: %v", ip, err)
		return
	}

	w.callWithTimeout("AddPath("+ip.String()+")", func() error {
		_, err := w.srv.AddPath(apiutil.AddPathRequest{
			Paths: []*apiutil.Path{{Family: family, Nlri: nlri, Attrs: pathAttrs}},
		})
		return err
	})
}

// Withdraw implements state.Announcer. DeletePath matches an existing
// path by NLRI *and* next-hop (confirmed by a real "nexthop not found"
// failure when Attrs was omitted) - attrs is state.Store's retained
// ipState.attrs for this IP (frozen at first announce), so the next-hop used
// here is guaranteed to match what Announce actually sent,
// including when NextHop was a per-rule override rather than the
// daemon's default.
func (w *Wrapper) Withdraw(ip state.IP, attrs state.RouteAttrs) {
	nlri, family, err := w.buildNLRI(ip)
	if err != nil {
		w.log.Warnf("bgp: building NLRI for withdraw of %s failed: %v", ip, err)
		return
	}
	pathAttrs, err := w.buildAttrs(ip, attrs)
	if err != nil {
		w.log.Warnf("bgp: building attributes for withdraw of %s failed: %v", ip, err)
		return
	}

	w.callWithTimeout("DeletePath("+ip.String()+")", func() error {
		return w.srv.DeletePath(apiutil.DeletePathRequest{
			Paths: []*apiutil.Path{{Family: family, Nlri: nlri, Attrs: pathAttrs}},
		})
	})
}

// buildNLRI dispatches on ip.IsIPv4() to build the host-route NLRI
// (/32 or /128). Confirmed via go doc on the real v4 module (not
// inferred by analogy this time): NewIPAddrPrefix is a single, unified
// constructor for both address families - it takes a netip.Prefix, which
// already carries its own version, and there is no separate
// NewIPv6AddrPrefix at all. The previous version's guess (a v6-specific
// constructor) was wrong; this replaces it.
//
// RF_IPv6_UC is still an unverified guess by naming symmetry with the
// confirmed RF_IPv4_UC (github.com/osrg/gobgp/v4/pkg/packet/bgp) - if the
// next build complains about it specifically, that constant name is the
// next thing to check.
func (w *Wrapper) buildNLRI(ip state.IP) (native.NLRI, native.Family, error) {
	if ip.IsIPv4() {
		nlri, err := native.NewIPAddrPrefix(netip.PrefixFrom(ipv4Addr(ip), 32))
		return nlri, native.RF_IPv4_UC, err
	}
	nlri, err := native.NewIPAddrPrefix(netip.PrefixFrom(ipv6Addr(ip), 128))
	return nlri, native.RF_IPv6_UC, err
}

// buildAttrs builds the mandatory attributes (Origin, NextHop) plus
// whatever state.RouteAttrs carries - community/large-community/AS-path/
// local-pref/MED are all optional and only added when actually set,
// rather than always emitting a zero-value attribute.
// resolveNextHop picks a next-hop matching ip's address family. RFC 4760
// requires an IPv6 NLRI's next-hop to itself be IPv6 - even when the
// session transport is IPv4, as in every deployment seen so far (BIRD
// confirmed this with a real rejection: "Invalid NEXT_HOP attribute -
// mismatched address family" when the IPv4 default was reused for a v6
// route). override is the per-rule next-hop for ip's family (pickNextHop), if any
// - also checked against ip's family, since a misconfigured rule could
// reintroduce exactly this bug for user-supplied values.
func (w *Wrapper) resolveNextHop(ip state.IP, override string) (netip.Addr, error) {
	def := w.cfg.NextHop
	if !ip.IsIPv4() {
		def = w.cfg.NextHopV6
	}

	candidate := override
	if candidate == "" {
		candidate = def
	}
	if candidate == "" {
		return netip.Addr{}, fmt.Errorf("no next-hop configured for address family of %s (NextHopV6 empty?)", ip)
	}

	addr, err := netip.ParseAddr(candidate)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid next-hop %q: %w", candidate, err)
	}
	if addr.Is4() != ip.IsIPv4() {
		w.log.Warnf("bgp: next-hop %s address family mismatches route %s, falling back to configured default", candidate, ip)
		if def == "" || def == candidate {
			return netip.Addr{}, fmt.Errorf("next-hop %q address family mismatches %s and no usable default", candidate, ip)
		}
		return w.resolveNextHop(ip, "") // retry with override cleared, forcing def
	}
	return addr, nil
}

func (w *Wrapper) buildAttrs(ip state.IP, attrs state.RouteAttrs) ([]native.PathAttributeInterface, error) {
	nextHop, err := w.resolveNextHop(ip, pickNextHop(ip, attrs))
	if err != nil {
		return nil, err
	}
	nextHopAttr, err := native.NewPathAttributeNextHop(nextHop)
	if err != nil {
		return nil, fmt.Errorf("building next-hop attribute: %w", err)
	}

	// AS_PATH is a well-known *mandatory* attribute (RFC 4271 §5.1.2),
	// not optional - always send it, empty (zero segments) is the
	// correct wire form for a locally-originated iBGP route with no
	// prepends, not "omit the attribute". The earlier version's
	// conditional add was wrong (found from the user asking why
	// `bgp_path:` showed empty rather than being absent from the
	// attribute set at all - BIRD tolerated the omission, but that's
	// leniency on BIRD's part, not something to rely on for other
	// downstream speakers).
	// A correct empty AS_PATH is an attribute with ZERO segments, not one
	// segment containing zero ASNs - the latter is exactly the "malformed
	// AS_PATH attribute - zero-length segment" BIRD rejected both routes
	// over. Only emit a segment when there's actually something in it.
	var segments []native.AsPathParamInterface
	if len(attrs.ASPath) > 0 {
		segments = []native.AsPathParamInterface{
			native.NewAs4PathParam(native.BGP_ASPATH_ATTR_TYPE_SEQ, attrs.ASPath),
		}
	}
	asPathAttr := native.NewPathAttributeAsPath(segments)

	out := []native.PathAttributeInterface{native.NewPathAttributeOrigin(0), nextHopAttr, asPathAttr}

	if attrs.HasLocalPref {
		out = append(out, native.NewPathAttributeLocalPref(attrs.LocalPref))
	}
	if attrs.HasMED {
		out = append(out, native.NewPathAttributeMultiExitDisc(attrs.MED))
	}
	if encoded := w.encodeCommunities(attrs.Communities); len(encoded) > 0 {
		out = append(out, native.NewPathAttributeCommunities(encoded))
	}
	if len(attrs.LargeCommunities) > 0 {
		lcs := make([]*native.LargeCommunity, 0, len(attrs.LargeCommunities))
		for _, lc := range attrs.LargeCommunities {
			lcs = append(lcs, native.NewLargeCommunity(lc.ASN, lc.Data1, lc.Data2))
		}
		out = append(out, native.NewPathAttributeLargeCommunities(lcs))
	}
	return out, nil
}

// callWithTimeout enforces the "never block the caller past
// AnnounceTimeout" contract without a cancellable context (v4's
// AddPath/DeletePath don't take one - see Config's AnnounceTimeout doc
// comment) - fn runs in its own goroutine; if it doesn't finish in time
// we stop waiting and log, but fn keeps running and will log its own
// outcome if/when it does return. This is a defensive backstop, not the
// common path: GoBGP's RIB mutations are normally fast.
func (w *Wrapper) callWithTimeout(op string, fn func() error) {
	done := make(chan error, 1)
	go func() { done <- fn() }()

	select {
	case err := <-done:
		if err != nil {
			w.log.Warnf("bgp: %s failed: %v", op, err)
		}
	case <-time.After(w.cfg.AnnounceTimeout):
		w.log.Warnf("bgp: %s exceeded %s - not waiting further; a goroutine is still finishing it in the background", op, w.cfg.AnnounceTimeout)
	}
}

// encodeCommunities packs state.Community values into RFC 1997 standard
// (16-bit ASN, 16-bit value) BGP communities - the format
// communities.conf.txt actually uses throughout this project. A value
// with either half outside 16 bits cannot be represented in that format
// at all - packing it anyway would silently wrap during the shift and
// announce a wrong, unrelated community on a live BGP session. Such
// values are dropped with a loud warning instead.
func (w *Wrapper) encodeCommunities(cs []state.Community) []uint32 {
	out := make([]uint32, 0, len(cs))
	for _, c := range cs {
		asn, val := c.ASN(), c.Value()
		if asn > 0xFFFF || val > 0xFFFF {
			w.log.Warnf("bgp: community (%d,%d) does not fit RFC1997 16+16 bits, dropped from announce", asn, val)
			continue
		}
		out = append(out, asn<<16|val)
	}
	return out
}

func ipv4Addr(ip state.IP) netip.Addr {
	return netip.AddrFrom4([4]byte{ip[12], ip[13], ip[14], ip[15]})
}

func ipv6Addr(ip state.IP) netip.Addr {
	return netip.AddrFrom16([16]byte(ip))
}

// SessionInfo reads the peer's current state from the embedded server. It
// is a status query (ctl stats / ctl bgp), never called on the DNS or
// announce path. Every protobuf access goes through the generated Get*
// accessors, which are nil-safe.
func (w *Wrapper) SessionInfo(ctx context.Context) (SessionInfo, error) {
	if w.srv == nil {
		return SessionInfo{}, fmt.Errorf("bgp: server not started")
	}
	var info SessionInfo
	found := false
	err := w.srv.ListPeer(ctx, &api.ListPeerRequest{Address: w.cfg.PeerAddress}, func(p *api.Peer) {
		found = true
		st := p.GetState()
		info.Established = st.GetSessionState() == api.PeerState_SESSION_STATE_ESTABLISHED
		info.State = strings.ToLower(strings.TrimPrefix(st.GetSessionState().String(), "SESSION_STATE_"))
		info.Flaps = st.GetFlops()
		if r := st.GetDisconnectReason(); r != 0 {
			info.DisconnectReason = strings.ToLower(strings.TrimPrefix(r.String(), "DISCONNECT_REASON_"))
		}
		info.DisconnectMessage = st.GetDisconnectMessage()
		if info.Established {
			if up := p.GetTimers().GetState().GetUptime(); up.GetSeconds() > 0 {
				info.UptimeSeconds = int64(time.Since(up.AsTime()).Seconds())
			}
		}
		sent := st.GetMessages().GetSent()
		recv := st.GetMessages().GetReceived()
		info.UpdatesSent = sent.GetUpdate()
		info.UpdatesReceived = recv.GetUpdate()
		info.WithdrawsSent = sent.GetWithdrawUpdate()
		info.NotificationsSent = sent.GetNotification()
		info.NotificationsReceived = recv.GetNotification()
	})
	if err != nil {
		return SessionInfo{}, fmt.Errorf("bgp: ListPeer: %w", err)
	}
	if !found {
		return SessionInfo{}, fmt.Errorf("bgp: peer %s not found", w.cfg.PeerAddress)
	}
	return info, nil
}
