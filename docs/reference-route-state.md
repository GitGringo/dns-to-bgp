# unbound-route-state reference

`unbound-route-state` is the Go daemon of dns-to-bgp. It receives route events from the Unbound Python module over a UNIX socket, keeps a table of live IP addresses with TTL-based expiry, classifies each route by domain pattern (communities and other BGP attributes) and announces or withdraws it over a BGP session (embedded GoBGP) to BIRD.

It is built from `internal/state` (route table, see [internals-state-store.md](internals-state-store.md)), `internal/bgp` (BGP wrapper, see [internals-bgp-wrapper.md](internals-bgp-wrapper.md)) and, inside `cmd/unbound-route-state`, the UNIX-socket transport and the wildcard-to-community classifier.

## Build

```bash
cd unbound-route-state
go build -o unbound-route-state ./cmd/unbound-route-state   # see docs/INSTALL.md for the size-optimised flags
```

## Deployment layout

```
/opt/dns-to-bgp/
    unbound-route-state/
        unbound-route-state             # binary
    conf.d/
        state.conf                      # from examples/conf.d/state.conf
        communities.conf                # from examples/conf.d/communities.conf
```

systemd: copy `examples/systemd/unbound-route-state.service` to `/etc/systemd/system/unbound-route-state.service`, then run `systemctl daemon-reload && systemctl enable --now unbound-route-state`. The unit runs the daemon as `User=unbound` (the same user as Unbound, so the Python module can write to the socket `/run/dns-to-bgp/state.sock`); the runtime directory is created by `RuntimeDirectory=`.

| Path | Owner | Mode |
|---|---|---|
| `/opt/dns-to-bgp`, `.../unbound-route-state`, `.../conf.d` | `root:root` | `0755` |
| `unbound-route-state` (binary) | `root:root` | `0755` |
| `conf.d/state.conf`, `conf.d/communities.conf` | `root:unbound` | `0640` |

The daemon only reads its configuration files (including during hot reload); it does not need write access to `/opt`.

## Command line

```
unbound-route-state                       # no arguments: usage and version
unbound-route-state daemon [options]      # run the daemon (as in the systemd unit)
unbound-route-state ctl [options] <cmd>   # query a running daemon
unbound-route-state version               # print the version (currently 2.0.0)
unbound-route-state help
```

`daemon --help` and `ctl --help` describe their own options. The version is also shown in `ctl stats` (section `process`) and in the daemon's startup log line.

`daemon` options:

| Option | Meaning |
|---|---|
| `--config FILE` | Path to the configuration file (default `/opt/dns-to-bgp/conf.d/state.conf`). |
| `-debug` | Log ANNOUNCE/WITHDRAW trace messages (domain, TTL) and GoBGP debug output (connection attempts, FSM). |

Before version 2.0 the daemon was started without a subcommand (`unbound-route-state --config ...`). That form still works but prints a deprecation warning; in the unit file, replace it with `daemon --config ...`.

## Configuration: state.conf

INI format: `[section]` headers, `key = value` pairs, full-line and inline comments with `#` or `;`. Booleans accept `yes`/`no`, `true`/`false`, `1`/`0`. Defaults below are the built-in values used when a key is absent; "-" means no default.

### [general]

| Key | Default | Description |
|---|---|---|
| `listen_socket` | - | UNIX socket on which the daemon receives events (example: `/run/dns-to-bgp/state.sock`). |
| `socket_mode` | `0660` | File mode of `listen_socket`. |
| `max_inbound_conns` | `2` | Maximum number of simultaneous connections on `listen_socket`. |
| `log_rate_limit_per_min` | `60` | Rate limit for WARN messages, see [Logging](#logging). `0` = unlimited. |
| `control_socket` | `/run/dns-to-bgp/control.sock` | Control socket for `ctl`. Empty value disables it. |
| `control_socket_mode` | `0660` | File mode of `control_socket`. |
| `track_fqdn` | `yes` | Keep the FQDN(s) for every live IP (used by `ctl routes`). Costs roughly one short string per (IP, domain) pair; a few MB for a table of tens of thousands of IPs. |
| `asn` | - | Local AS number. |
| `router_id` | - | BGP router ID. |
| `local_address` | - | Local address of the BGP session. |
| `next_hop` | empty | Default next-hop for IPv4 routes. Empty = `local_address`. |
| `next_hop_v6` | empty | Default next-hop for IPv6 routes. Required for any IPv6 announcement; empty = IPv6 routes are not announced. |
| `neighbor` | - | BGP neighbor (BIRD) address. |
| `neighbor_asn` | - | Neighbor AS number. |
| `passive` | - | `yes`: the daemon listens and BIRD connects to it. `no`: the daemon connects to BIRD. |
| `remote_port` | - | Remote port used when `passive = no`; not used when `passive = yes`. |
| `hold_time_seconds` | `240` | BGP hold time. |
| `connect_retry_seconds` | `10` | How often the daemon retries connecting while the session is down (active mode only; minimum 2). |
| `idle_hold_seconds` | `5` | Pause before reconnecting after a session reset (active mode only; minimum 1). |
| `graceful_restart` | `yes` | Enable BGP graceful restart. |
| `graceful_restart_time_seconds` | `300` | Graceful restart time. |
| `llgr_enabled` | `yes` | Enable long-lived graceful restart. |
| `llgr_stale_time` | `600` | LLGR stale time. |

### [limits]

| Key | Default | Description |
|---|---|---|
| `max_ip_entries` | `50000` | Maximum number of IPs in the route table. |
| `event_queue_size` | `4096` | Buffer of the channel between the socket reader and the Store. When it is full, events are dropped (never blocked or retried) and the drop is logged. Raise it if `events channel full` appears under normal load. |
| `max_domain_patterns` | `5000` | Limit on the total number of patterns that come from `domains.d` list files referenced by `communities.conf`. |

### [ttl]

| Key | Default | Description |
|---|---|---|
| `granularity_seconds` | `1` | Timing-wheel granularity. |
| `min_route_ttl_seconds` | `0` | Minimum route lifetime regardless of the DNS TTL. |

### [communities]

| Key | Default | Description |
|---|---|---|
| `rules_file` | - | Path to `communities.conf` (example: `/opt/dns-to-bgp/conf.d/communities.conf`). |
| `rules_reload_seconds` | `5` | Hot-reload check period, see [Hot reload](#hot-reload-of-community-rules). `0` = off. |
| `domains_dir` | `/opt/dns-to-bgp/domains.d` | Directory of `*.list` files that block headers may reference. |

### [keepalive] and [unbound]

Described in [Keeping routes while traffic flows](#keeping-routes-while-traffic-flows-keepalive) and [Flushing the Unbound cache](#flushing-the-unbound-cache-at-start).

## Rules: communities.conf

A block is `[pattern1, pattern2, ...]` followed by optional keys. One FQDN can match several blocks: `community` and `large_community` accumulate over all matching blocks, while `next_hop`, `next_hop_v6`, `as_path`, `local_pref` and `med` come from the first matching block in file order. `*.x` does not match the apex `x`.

```ini
[*.cdn.example.net, *.media.example.org]
community = 64512:2780

[*.example.com]
community        = 64512:2780, 64512:9000
large_community  = 64512:1:100
next_hop         = 192.0.2.3
next_hop_v6      = 2001:db8:114:115::3
as_path          = 64513
local_pref       = 150
med              = 10

[default:missed]
community = 64512:9500
```

| Key | Meaning |
|---|---|
| `community` | `ASN:VALUE[, ASN:VALUE ...]` |
| `large_community` | `ASN:DATA1:DATA2[, ...]` |
| `next_hop` | IPv4 address (for A routes) |
| `next_hop_v6` | IPv6 address (for AAAA routes) |
| `as_path` | `ASN[, ASN ...]` |
| `local_pref` | integer |
| `med` | integer |

`[default:missed]` is a special block, not a pattern. It applies only to an FQDN that matched no other block, for example when `domains.d/*.list` and `communities.conf` disagree and the Python module sends a domain that `communities.conf` does not know. Without this block such a route is announced with no community and is invisible to community-based export filters on the receiving router; the block makes the mismatch visible and classifiable.

### Hot reload of community rules

`rules_file` is re-read without a restart every `rules_reload_seconds` (default 5, `0` = off; the check compares mtime and size). The rule set is replaced atomically as a whole. A parse error, a missing file, or a result with no rules at all (a truncated file) is rejected and the previous rules stay in effect. Routes that are already announced are not recomputed: new rules apply to events that arrive after the swap. Edit the file atomically (write a temporary file, then `mv`).

### List files in a block header

An entry in a block header can be not only a pattern but also the name of a file from `domains.d` (an entry ending in `.list`). The block then covers all patterns in that file, so the domain list is maintained in one place: the same `domains.d/corp.list` that the announcer reads defines the domains of the block.

```ini
[corp.list, *.extra.example.com]
community       = 64512:112, 64512:113
large_community = 64512:1:112
next_hop        = 192.0.2.3
```

- The directory is `domains_dir` in `[communities]` (default `/opt/dns-to-bgp/domains.d`). Only a plain file name inside the directory is accepted (`../x.list`, `sub/x.list` and `.hidden.list` are rejected).
- Patterns are substituted in the order of the header. The block behaves as if all patterns had been listed in it by hand (communities accumulate, other attributes from the first match, `*.x` does not match the apex `x`).
- Hot reload watches `communities.conf` and every referenced file (mtime and size). Editing a list, a missing list appearing, or a list being deleted changes the rules without a restart, exactly like `communities.conf` itself (`rules_reload_seconds`). An invalid version is rejected and the previous rules stay.
- A missing file is not an error (lists are filled externally): it is treated as empty, one `WARN` is logged, and the file is picked up when it appears. Lines that are not a hostname or `*.hostname` are skipped and counted in the same `WARN`. CRLF line endings are accepted.
- Safety limit: the total number of patterns that come from files is capped by `max_domain_patterns` (`[limits]`, default 5000); a reload that exceeds it is rejected. Patterns written directly in a header are not limited.
- `ctl rules` shows a file with a counter (`corp.list(10)` or `corp.list(missing)`) instead of the expanded list; `--json` has the fields `list_files` and `pattern_count`. `ctl flush` and `flush_on_start` take the patterns from these files into account.
- This affects route attributes only. What is announced at all is still decided by the announcer from `domains.d`; a domain from a list that no block refers to gets the attributes of `[default:missed]`.

### Next-hop per address family

BGP requires the next-hop to be of the same family as the NLRI, so both the defaults and the per-rule overrides are set separately for IPv4 and IPv6:

| Where | IPv4 (A) | IPv6 (AAAA) |
|---|---|---|
| `state.conf` `[general]`, default | `next_hop` (empty = `local_address`) | `next_hop_v6` (empty = IPv6 not announced) |
| `communities.conf`, in a block | `next_hop` | `next_hop_v6` |

If the matching block does not set the key for the route's family, the default from `state.conf` is used. An address of the wrong family in a block key is not applied, and one summary `WARN` is logged on load and on every hot reload. For compatibility with older files, an IPv6 address in `next_hop` is still honoured for IPv6 routes when `next_hop_v6` is not set. In `state.conf`, an address of the wrong family in `next_hop` / `next_hop_v6` is rejected at startup. `ctl route` and `ctl rules` show both keys.

### Pitfall: as_path and the ASN of BIRD itself

If a rule's `as_path` contains an ASN equal to `neighbor_asn` (the AS of BIRD itself), BIRD treats the route as an AS-path loop and silently drops it; it simply does not appear in the table. The daemon checks this at startup and on every rule hot reload and logs one summary `WARN` (`as_path contains AS... in rule(s) [...]`), but does not reject the rule. Either remove `as_path`, use a different ASN, or enable `allow local as` in BIRD (which weakens loop protection for the whole session).

## Statistics and state inspection (ctl)

The daemon listens on a separate control socket (`control_socket`, default `/run/dns-to-bgp/control.sock`, mode `0660`, owned by the daemon's user). It is independent of the data socket, so a slow `ctl routes` does not hold up event intake. An empty `control_socket` disables the feature.

```
unbound-route-state ctl [-socket PATH] [-config FILE] <command> [flags]
```

Help: `unbound-route-state --help` (daemon flags and the `ctl` subcommand), `unbound-route-state ctl --help` (options and command list) and `unbound-route-state ctl <command> --help` (command flags). Help is printed locally, needs no running daemon, and exits with code 0.

Without `-socket`, the path is taken from `control_socket` in `-config` (default `/opt/dns-to-bgp/conf.d/state.conf`). To run as a user other than the daemon's, add yourself to its group (`unbound`) or use `sudo`. Equivalent without the binary: `echo "stats --json" | socat - UNIX-CONNECT:/run/dns-to-bgp/control.sock`.

| Command | What it shows |
|---|---|
| `stats [--json]` | Counters: process, intake, queue, routes, rules, BGP |
| `routes [--ip IP] [--fqdn TEXT] [--sort ttl\|age\|ip\|renewals] [--limit N] [--json \| --json-lines]` | Current route cache |
| `route IP [--json]` | One route in detail |
| `rules [--json]` | Blocks of `communities.conf` and their hit counts |
| `bgp [--json]` | BGP session state |
| `flush [NAME] [--zone] [--with-domains-dir] [--dry-run] [--json]` | Flush names from the Unbound cache on demand (see [Flushing the Unbound cache](#flushing-the-unbound-cache-at-start)) |

The default output is aligned text (convenient for `grep` and `awk`); `--json` is a stable schema with `schema_version`; `--json-lines` prints one route per line. `ctl` exit codes: 0 = ok, 1 = error from the daemon (the reply starts with `ERROR: `), 2 = could not connect.

Things to know:

- **Per-route `renewals` counter.** How many times the IP received an event while it was in the cache (a TTL renewal by the same domain, or a new domain joining; both count). `stats` reports these separately as `renewals_total` and `domain_joins_total`.
- **The FQDN column in `routes`** is always last (it is the only variable-width column). Values are present only if `track_fqdn = yes` (the default). They are stored in a separate table, so `ipState` does not grow.
- **Rule hits** are counted from the load of the current rule generation and reset on every hot reload (`generation` in `ctl rules`).
- **The `stats` summary** needs no disk or external database: counters live in memory and reset on restart. For trends, collect `ctl stats --json` with an external system (for example a Zabbix `UserParameter`).
- **The table snapshot** is taken inside the Store goroutine (the sole owner of the table), so no locks are added to the event path. The cost is one pass over the table, during which events wait briefly in the queue.
- **History of withdrawn routes** is not implemented (`routes --history` returns an error). There is an extension point: `state.Config.OnWithdraw` receives the `state.RouteInfo` of every route leaving the cache; a ring buffer of that type could be attached without changing the JSON schema or the socket protocol.

## BGP session recovery after a restart

When the daemon is the active side (`passive = no`), it dials BIRD itself. The time to return to `Established` is the sum of two waits:

1. **BIRD side.** On shutdown the daemon sends NOTIFICATION `Cease / Peer De-configured` (code 6/3). BIRD logs `Idle` with the reason `Received: Peer de-configured` and waits before starting the session again (`error wait time` in the BIRD configuration; by default it grows from 60 to 300 s on repeated consecutive errors, so a series of restarts lengthens the pause).
2. **Daemon side.** While BIRD is not ready to accept the connection, GoBGP retries every `connect_retry_seconds`. The GoBGP default is 120 s (in practice 90-120 s between attempts), so even after BIRD is ready the daemon could wait up to two more minutes.

To see the connection attempts themselves, run the daemon with `-debug`: GoBGP then logs `try to connect` / `failed to connect`. At the normal log level these attempts are not logged, so silence in the log proves nothing.

The defaults are `connect_retry_seconds = 10` and `idle_hold_seconds = 5` (a pair of keys in `[general]`), so the second wait is practically gone. The first can only be tuned on the BIRD side: see `birdc show protocols all <protocol>` (fields `Last error` and `Error wait`) and the `error wait time` / `error forget time` options in `protocol bgp`.

## Flushing the Unbound cache at start

Unbound answers repeated queries from its cache **without calling modules**. After a daemon restart the route table is empty, and cached names do not return to it until their TTL expires, so the BGP feed gets gaps. With `flush_on_start = yes` (section `[unbound]`) the daemon calls `unbound-control` once at startup, and the next client query for those names goes through the Python module again.

| Key `[unbound]` | Default | Description |
|---|---|---|
| `flush_on_start` | `no` | Flush once at daemon start. |
| `control_command` | `/usr/sbin/unbound-control` | How to call `unbound-control`. |
| `control_args` | empty | Extra arguments placed before the command, space-separated (for example `-c` with the path to `unbound.conf`, or `-s` with a remote-control address). |
| `flush_domains_dir` | `/opt/dns-to-bgp/domains.d` | Directory of `*.list` files to take names from. Empty = only patterns from `communities.conf`. |
| `flush_max_commands` | `500` | Upper bound on the number of commands per flush. |
| `flush_pause_ms` | `20` | Pause between commands. |

- Names come from `*.list` files in `flush_domains_dir` and from the patterns of `communities.conf`. `*.example.com` becomes `flush_zone example.com` (which also covers `example.com`); an exact name becomes `flush example.com`. Nested and already-covered names are dropped; names with invalid characters are skipped.
- The flush runs after the Python module's socket accepts connections and after an `unbound-control status` check (up to 10 attempts, 2 s apart, since the daemon may start before Unbound). If `unbound-control` is unavailable, one `WARN` is logged and the daemon keeps running.
- `flush_zone` walks the whole cache and is documented by Unbound as slow, hence the `flush_max_commands` limit and the `flush_pause_ms` pause. Flushing raises the load on upstream resolvers right after a restart: routes fill in as clients query.
- Permissions: the daemon's user must be able to read the Unbound remote-control keys (`control-enable: yes`, access to `unbound_control.key/.pem`, or to the unix socket via `control-interface`). Check manually as the same user: `sudo -u unbound unbound-control status`.
- The result is visible in `ctl stats` (section `unbound_flush`) and in the log.
- The automatic flush runs only at daemon start, not when `domains.d` or `communities.conf` is reloaded.

### On-demand flush: ctl flush

The same mechanism, run manually. It works with `flush_on_start = no` too. The daemon runs the commands as its own user, so it is the daemon that needs access to `unbound-control`.

```
ctl flush                        # all patterns from communities.conf
ctl flush example.com            # one exact name          -> unbound-control flush
ctl flush '*.example.com'        # a whole zone            -> unbound-control flush_zone
ctl flush example.com --zone     # same as '*.example.com'
ctl flush --with-domains-dir     # plus *.list files from flush_domains_dir (the flush_on_start set)
ctl flush --dry-run              # show the commands without running them
ctl flush ... --json             # machine-readable result
```

Only one flush runs at a time, and the number of commands is capped by `flush_max_commands`. The name is validated strictly (only a plain hostname or `*.hostname`). If at least one command fails, the reply starts with `ERROR:` and the `ctl` exit code is 1. Counters appear in `ctl stats` (section `unbound_flush`).

## Keeping routes while traffic flows ([keepalive])

A client may keep using an IP (TCP, QUIC, NAT-ed UDP) after the DNS TTL that brought the IP into the cache has expired. If the route is withdrawn at that moment, the session's packets take a different path. `min_route_ttl_seconds` addresses this bluntly (all routes are held long, needed or not). With `[keepalive] enabled = yes` the daemon checks whether there is live traffic to the IP:

1. Every `poll_interval_seconds` it takes the list of IPs from its cache.
2. It runs `conntrack -L -f ipv4` (and `-f ipv6` if the cache contains IPv6) and parses the output as a stream. A flow is a record whose **original destination address** (or reply source address) equals an IP from the cache; this also works behind NAT, where client traffic passes through the server.
3. For the IPs found, it tells the Store "flow seen now"; the route is extended to `lease_seconds` after the last observation, but not beyond `max_hold_seconds` for a single series of extensions.

Route lifetime is at least the normal term (DNS TTL / `min_route_ttl_seconds`); beyond that, as long as there is traffic plus `lease_seconds`, but a series of extensions is no longer than `max_hold_seconds`. A new DNS answer for the IP starts the series over.

**What counts as a live flow.** TCP in the states SYN_SENT, SYN_RECV and ESTABLISHED (TIME_WAIT and closing states do not count). Any other protocol (UDP, ICMP, ...) counts while the entry exists in the table: these have short kernel timeouts, so an entry means recent traffic. By default (`tcp_idle_limit_seconds = 0`) idle TCP connections are governed by the kernel's own timeout `nf_conntrack_tcp_timeout_established` (the daemon reads it from `/proc/sys/net/netfilter/`). To be stricter, set `tcp_idle_limit_seconds = N`: a connection then counts as active only if it carried a packet in the last N seconds (idle time is computed as the kernel timeout minus the remaining entry lifetime; the `nf_conntrack_acct` counters are not needed).

| Key `[keepalive]` | Default | Description |
|---|---|---|
| `enabled` | `no` | Enable the feature. |
| `conntrack_command` / `conntrack_args` | `/usr/sbin/conntrack` / empty | How to invoke the utility. |
| `poll_interval_seconds` | 15 | Poll period (minimum 2). |
| `lease_seconds` | 600 | How long to hold after the last observation. |
| `max_hold_seconds` | 86400 | Ceiling for one series of extensions (0 = no ceiling). |
| `tcp_idle_limit_seconds` | 0 | 0 = kernel timeout, >0 = stricter. |
| `tcp_established_timeout_seconds` | 0 | 0 = read from the kernel. |

Requirements and notes:

- **Privileges.** The `conntrack` package is required (`apt install conntrack`), and the daemon needs `CAP_NET_ADMIN`: the unit file sets `AmbientCapabilities=CAP_NET_ADMIN` (the child process inherits it). To verify as the service, start the daemon and watch `ctl stats` (`polls ok / failed`, `last_error`).
- **Minimum route lifetime.** So that at least one poll sees a new route, with keepalive enabled every route lives at least `2 * poll_interval_seconds` (raised automatically; nothing changes if `min_route_ttl_seconds` is larger). Like `min_route_ttl_seconds`, this affects only the internal withdrawal schedule.
- **On poll failure** (no permission, no utility) one `WARN` is logged and behaviour is the same as without keepalive: the DNS TTL and `min_route_ttl_seconds` decide the lifetime.
- **Load.** A poll reads the whole conntrack table as a stream (memory does not grow); with 5-7 thousand entries it takes milliseconds. For very large tables (hundreds of thousands of entries) increase `poll_interval_seconds`.
- **Visibility.** `ctl routes`: column `LAST_FLOW` (age of the last observed flow; `*` = the route is alive only because of traffic). `ctl route IP`: `last_traffic`, `held_by_traffic`, `dns_expires_at`. `ctl stats`: section `keepalive` (polls, errors, number of held routes, extensions). The same fields appear in `--json`.
- Polling was chosen over conntrack events (`conntrack -E`) because withdrawal decisions are made with minute-level precision, while events can be lost under load and still require an initial dump.

## Logging

`log_rate_limit_per_min` (`[general]`, default 60, `0` = unlimited): each kind of WARN (keyed by its format string) is emitted at most once per 60/N seconds; repeats are collapsed into `[+N similar message(s) suppressed]`. `INFO` is not limited.

## Design notes and limitations

- **The community classifier is a linear list, not a reversed-label trie.** A trie is needed on the hot path of DNS resolution inside Unbound (the Python module, thousands of queries per second). Here it is a separate, much less loaded copy on the Go daemon side; a linear pass over the expected small number of rules (`max_domain_patterns`, default 5000) should not be a bottleneck.
- **No snapshot persistence.** The daemon always starts with an empty state. Graceful restart and long-lived graceful restart on the BIRD side are the only protection against losing routes when the daemon restarts. A snapshot could be added later without changing the `Announcer`/`Store` contract.
- **`WheelSlots` is hard-coded to 3600** (a one-hour horizon at 1 s granularity) and is not a configuration key.
- **The INI parser is hand-written** for this project's format, to avoid an extra external dependency for about fifteen keys.
