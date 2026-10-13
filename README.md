# dns-to-bgp

Route traffic by **domain name** when the destination IP addresses are not known in advance.

Many services (CDNs, banks, video platforms) resolve to changing sets of IP addresses, so a static route list is never complete. dns-to-bgp watches the DNS answers that pass through your [Unbound](https://nlnetlabs.nl/projects/unbound/) resolver. For every answer that matches your domain lists, it announces the resolved IPs as `/32` and `/128` host routes over BGP to [BIRD](https://bird.network.cz/) for the lifetime of the DNS TTL. Downstream routers (for example MikroTik) can then send exactly that traffic through a chosen path, such as a tunnel, without knowing the addresses in advance.

[Русская версия](README.ru.md)

```
 client ── DNS query ──▶ Unbound ──▶ upstream resolver
                           │ (python module)
                           │  matched A/AAAA answers (UNIX socket, NDJSON)
                           ▼
                   unbound-route-state ── iBGP /32, /128 + communities ──▶ BIRD ──▶ routers
                   (route table, TTL expiry,
                    attributes from rules)
```

## Components

| Component | Language | Role |
|---|---|---|
| [`unbound-bgp-announcer`](unbound-bgp-announcer) | Python | Module loaded into Unbound. Matches answers against wildcard domain lists (hot-reloaded) and forwards events over a UNIX socket. Never blocks DNS. |
| [`unbound-route-state`](unbound-route-state) | Go | Daemon. Keeps the table of live IPs, expires them at the DNS TTL, assigns BGP attributes from rules, and announces routes with an embedded [GoBGP](https://github.com/osrg/gobgp). Includes the `ctl` inspection tool. |

## Features

- Wildcard domain lists in a directory, filled by any external process and hot-reloaded without restarting Unbound.
- Per-domain BGP attributes: communities, large communities, next hop (separately for IPv4 and IPv6), AS path, local preference, MED. Rules hot-reload too.
- IPv4 and IPv6 over one BGP session; graceful restart and long-lived graceful restart.
- Optional: keep a route announced while traffic to its IP still flows (via conntrack), and flush matching names from Unbound's cache on start or on demand.
- Introspection with `unbound-route-state ctl` (stats, routes, rules, BGP state), plain text or JSON.
- Small and defensive: one static binary, bounded queues and tables, rate-limited logs, a memory footprint of a few tens of MB.

## Quick start

1. Build: `make build`
2. Follow the [installation and configuration guide](docs/INSTALL.md) ([на русском](docs/INSTALL.ru.md)).
3. Example configuration for every component is in [`examples/`](examples).

## Documentation

- [Installation and configuration guide](docs/INSTALL.md) / [на русском](docs/INSTALL.ru.md)
- [`unbound-route-state` reference](docs/reference-route-state.md): all configuration keys, rules format, `ctl`
- [`unbound-bgp-announcer` reference](docs/reference-announcer.md)
- Internals: [route table](docs/internals-state-store.md), [BGP wrapper](docs/internals-bgp-wrapper.md)

## Limitations

- The daemon starts with an empty route table (there is no persistence); use graceful restart in BIRD to bridge daemon restarts.
- Only host routes (`/32`, `/128`) are announced.
- Linux only (the optional keepalive uses conntrack).

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md). Changes are listed in [CHANGELOG.md](CHANGELOG.md).

## License

[MIT](LICENSE)
