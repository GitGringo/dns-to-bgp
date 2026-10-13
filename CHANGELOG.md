# Changelog

All notable changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [2.0.0]

First public release.

### Features
- Unbound Python module `unbound-bgp-announcer`: wildcard domain matching with
  hot-reloaded `domains.d/*.list`, NDJSON events over a UNIX socket, lazy
  re-initialisation, one resend on a fresh connection.
- Go daemon `unbound-route-state`: TTL-based route table, embedded GoBGP session
  to BIRD (IPv4 and IPv6, graceful restart / LLGR), rule-based BGP attributes
  from `communities.conf` (communities, large communities, next hop per family,
  AS path, local-pref, MED) with hot reload and `*.list` references.
- `ctl` subcommands: `stats`, `routes`, `route`, `rules`, `bgp`, `flush`.
- Optional traffic keepalive via conntrack and optional Unbound cache flush.
