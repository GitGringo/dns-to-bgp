# Internals: state store (internal/state)

The route table of `unbound-route-state`: IP deduplication across domains, TTL expiry through a timing wheel, and a single-owner (actor) model. The package has no external dependencies; the BGP announcer and logger are interfaces (`Announcer`, `Logger`) supplied by the caller. The BGP implementation lives in `internal/bgp` (see [internals-bgp-wrapper.md](internals-bgp-wrapper.md)).

## Build

```
go build ./...
go vet ./...
```

## Memory

Each IP entry costs on the order of 200 bytes in practice, including Go allocator overhead (size classes, GC metadata). At the default `max_ip_entries` of 50 000 this is about 10 MB. A renewal (an event for an IP that is already in the table) does not allocate. If memory ever needs to shrink further, the first candidate is pooling `ipState` domain slices (`sync.Pool`).

## Design notes

1. **Timing-wheel links are pointers, not slice indices.** The wheel could use indices into a slice, but the top-level `byIP` map is `map[[16]byte]*ipState`, so every `ipState` is already a separate heap allocation. A full move to a slice plus free-list arena with generation-safe indices would save about 8 bytes per entry at the price of much more complexity, so `*ipState` pointers (`wheelNext` / `wheelPrev`, see `types.go`) are used.

2. **Communities are frozen at the first announce, with no re-announce.** If a second domain joins an IP that is already announced and carries communities that were absent at the first announce, those communities do not reach the live BGP path (see the comment at `handleEvent` in `store.go`). This is a rare case for CDN edges (normally one physical IP is one service) but not an impossible one.

3. **`AnnounceCoalesce` is declared in the config but not applied.** The mechanism was designed for a "one `time.Timer` per IP" scheme, where frequent renewals meant repeated O(log n) re-arms in the runtime timer heap. The timing wheel already gives an O(1) reschedule (a linked-list splice), so the rationale for coalescing is much weaker. The field is kept in `Config` and is unused.

4. **The wheel catches up with real time.** `handleTick()` advances the wheel's internal clock in a loop (`wheel.tick()`) until it reaches `s.nowFn()`, rather than by one `granularity` step per call. Go's `time.Ticker` does not accumulate missed ticks; it drops them if the receiver is slow. Under sustained load on `s.events` the single-owner goroutine reaches `<-ticker.C` less often, and a one-step-per-call clock would fall behind real time permanently. After a full revolution the catch-up uses a capped `fastForward`, which protects against a long spin after a stall longer than the horizon (for example suspend/resume on an embedded device).

## IPv6 and RouteAttrs

- **IP is `[16]byte`**, not `uint32`: one 16-byte storage for IPv4 and IPv6 alike. The alternative, separate parallel structures, was rejected for code simplicity. IPv4 entries therefore also pay 16 bytes instead of 4, a deliberate trade-off.
- **`[]Community` in `Announcer` / `Event` / `ipState` is replaced by `RouteAttrs`** (community, large community, next-hop, AS-path, local-pref, MED). Communities and large communities accumulate over all matching rule blocks (see `classify()` in `cmd/unbound-route-state/communities.go`); the other attributes come from the first match. The merge happens entirely in `cmd/unbound-route-state`; this package only stores and passes on the finished `RouteAttrs` and takes no part in building it.
- **`Announcer.Withdraw` takes `(ip, attrs)`, not just `ip`.** The reason is practical: `internal/bgp` found (through a real GoBGP "nexthop not found" error) that a withdraw must match the path by NLRI **and** next-hop. The next-hop can be overridden per rule via `RouteAttrs.NextHop`, and `Withdraw` can no longer reconstruct it from the IP alone. `state.Store` keeps `ipState.attrs` for the whole lifetime of the entry anyway, so passing it adds no complexity.
