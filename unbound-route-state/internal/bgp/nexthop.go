package bgp

import (
	"net/netip"

	"unbound-route-state/internal/state"
)

// pickNextHop chooses which per-rule next-hop override applies to a route
// of ip's address family - one explicit key per family, mirroring the two
// defaults in Config (NextHop for IPv4, NextHopV6 for IPv6):
//
//   - IPv4 route: attrs.NextHop (communities.conf "next_hop").
//   - IPv6 route: attrs.NextHopV6 (communities.conf "next_hop_v6"). For
//     backward compatibility with rule files written before next_hop_v6
//     existed, an IPv6 literal in "next_hop" is still honoured for IPv6
//     routes when next_hop_v6 is absent.
//
// An empty result means "no override, use the configured default for this
// family". Pure function (no GoBGP types) so it is unit-testable.
func pickNextHop(ip state.IP, attrs state.RouteAttrs) string {
	if ip.IsIPv4() {
		return attrs.NextHop
	}
	if attrs.NextHopV6 != "" {
		return attrs.NextHopV6
	}
	if a, err := netip.ParseAddr(attrs.NextHop); err == nil && a.Is6() {
		return attrs.NextHop // legacy: IPv6 address placed in next_hop
	}
	return ""
}
