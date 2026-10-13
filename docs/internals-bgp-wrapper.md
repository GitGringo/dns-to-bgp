# Internals: BGP wrapper (internal/bgp)

A wrapper around GoBGP used as an embedded library (no `gobgpd` or `gobgp` CLI). It implements the `state.Announcer` interface from `internal/state` (see [internals-state-store.md](internals-state-store.md)).

## Target version: GoBGP v4.7.0

```go
require github.com/osrg/gobgp/v4 v4.7.0
```

**Requires Go 1.24.5 or newer.** Distribution packages may ship an older Go (for example `golang-go` on Ubuntu 24.04 installs 1.22, and `golang-1.24-go` from the distribution repositories can be one patch release short). Use the official tarball (`https://go.dev/dl/go1.24.5.linux-amd64.tar.gz`) or any newer Go 1.24.x / 1.25.x. Building also needs access to the Go module proxy (`proxy.golang.org`).

## Build

```bash
# if needed:
wget https://go.dev/dl/go1.24.5.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.24.5.linux-amd64.tar.gz
export PATH=/usr/local/go/bin:$PATH

cd unbound-route-state
go mod tidy      # regular proxy.golang.org, no replace directives
go build ./...
go vet ./...
```

## Implementation notes

- The GoBGP configuration structures used here (`Peer`, `PeerConf`, `GracefulRestart`, `AfiSafi`, `Timers`) are protobuf-generated. Long-lived graceful restart is configured per AFI-SAFI, not globally, and passive mode is `api.Transport.PassiveMode`.
- `apiutil.DeletePathRequest` / `DeletePath` returns a single `error`, unlike `AddPath`, which returns `(response, error)`. `Withdraw` in `wrapper.go` accounts for this.
- A withdraw must match the path by NLRI and next-hop (GoBGP reports "nexthop not found" otherwise), which is why `Announcer.Withdraw` receives the route's `RouteAttrs`. Next-hop selection per address family is done by `pickNextHop` in `nexthop.go`.

## Session port: 1790, not 179

`Config.ListenPort()` returns 1790 by default: the daemon does not listen on the privileged port 179 (see the comment in `Start`, `wrapper.go`). One change is needed on the BIRD side: in the protocol that accepts this session, add `port 1790` to the `neighbor` line:

```
neighbor 192.0.2.11 port 1790 as 64512;
```

Run `birdc configure` after the change.
