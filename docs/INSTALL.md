# Installation and configuration guide

[Русская версия](INSTALL.ru.md)

This guide takes a Linux server that already runs Unbound and BIRD from nothing to a working DNS-to-BGP setup. Read the [README](../README.md) first if you want to know what the project does.

All addresses and AS numbers in this guide are documentation placeholders (`192.0.2.0/24`, `2001:db8::/32`, private AS `64512`). Replace them with your own.

## 1. Requirements

| Component | Requirement |
|---|---|
| OS | Linux with systemd (tested on Ubuntu 24.04) |
| Unbound | Built with the Python module. Check: `unbound -V` must list `python` under "Linked modules" (Debian/Ubuntu packages do). |
| BIRD | 2.x, the BGP speaker that receives the routes |
| Go | 1.24+ to build the daemon (older Go 1.21+ downloads the required toolchain automatically) |
| conntrack | Only for the optional traffic keepalive: `apt install conntrack` |

The Unbound module runs inside the `unbound` process. The daemon runs as the same user (`unbound`) so that both can use one UNIX socket.

## 2. Get the sources and build

```bash
git clone https://github.com/GitGringo/dns-to-bgp.git
cd dns-to-bgp
make build            # produces bin/unbound-route-state
```

`make build` runs `go build` with `CGO_ENABLED=0 -trimpath -ldflags="-s -w"` (a small static binary). Without `make`:

```bash
cd unbound-route-state
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../bin/unbound-route-state ./cmd/unbound-route-state
```

## 3. Install the files

The expected layout (all paths below are the built-in defaults):

```
/opt/dns-to-bgp/
    unbound-bgp-announcer/      Python module (loaded by Unbound)
    unbound-route-state/        the daemon binary
    conf.d/                     announcer.conf, state.conf, communities.conf
    domains.d/                  *.list files with domain patterns
/run/dns-to-bgp/                sockets (created by systemd)
```

Either run `sudo make install` (copies everything, creates the directories, never overwrites existing configs), or do it by hand:

```bash
sudo install -d /opt/dns-to-bgp/{unbound-bgp-announcer,unbound-route-state,conf.d,domains.d}
sudo install -m 0644 unbound-bgp-announcer/*.py /opt/dns-to-bgp/unbound-bgp-announcer/
sudo install -m 0755 bin/unbound-route-state    /opt/dns-to-bgp/unbound-route-state/
sudo install -m 0640 examples/conf.d/*.conf     /opt/dns-to-bgp/conf.d/
sudo install -m 0644 examples/domains.d/*.list  /opt/dns-to-bgp/domains.d/
sudo install -m 0644 examples/systemd/unbound-route-state.service /etc/systemd/system/
```

Then set ownership. The configs and the domains directory must be readable by `unbound`:

```bash
sudo chown root:unbound /opt/dns-to-bgp/conf.d/*.conf /opt/dns-to-bgp/domains.d /opt/dns-to-bgp/domains.d/*.list
sudo chmod 0750 /opt/dns-to-bgp/conf.d && sudo chown root:unbound /opt/dns-to-bgp/conf.d
```

## 4. Configure the daemon: `conf.d/state.conf`

Edit the `[general]` section; the rest can stay at the defaults at first.

| Key | What to set |
|---|---|
| `asn` | Your local AS number. |
| `router_id` | A unique BGP router ID (an IPv4-looking value). |
| `local_address` | The address the daemon uses for the BGP session. |
| `neighbor`, `neighbor_asn` | The address and AS of BIRD. |
| `passive` | `yes` (default in the example): the daemon listens on TCP port 1790 and BIRD connects to it. |
| `next_hop` | Default next hop of IPv4 routes; empty = `local_address`. Usually the gateway that traffic for these hosts should go through. |
| `next_hop_v6` | Default next hop of IPv6 routes. **Required for any IPv6**: if it is empty, IPv6 routes are not announced. |

Every key is described in [docs/reference-route-state.md](reference-route-state.md).

## 5. Choose the domains

Domain lists live in `/opt/dns-to-bgp/domains.d/*.list`, one pattern per line:

```
*.cdn.example.net     # subdomains only, never cdn.example.net itself
example.org           # the apex only, never its subdomains
```

The announcer only reports DNS answers that match a pattern from these files. Anything may write into the directory (a cron job, a downloader); the files are re-read every 5 seconds without restarting Unbound. Write atomically: create a temporary file that does not end in `.list` and `mv` it into place.

## 6. Choose the route attributes: `conf.d/communities.conf`

This file says which BGP attributes (communities, next hop, local-pref, ...) a route gets, based on the domain. A block header lists patterns and/or `NAME.list` files from `domains.d`:

```ini
[corp.list, *.extra.example.com]
community = 64512:200

[default:missed]
community = 64512:999
```

Use the communities in BIRD filters (or in downstream routers) to decide what happens to these routes. Details, all keys and the pitfalls (notably: never put BIRD's own ASN into `as_path`) are in the [reference](reference-route-state.md#rules-communitiesconf). The file is hot-reloaded.

## 7. Announcer configuration: `conf.d/announcer.conf`

The defaults already match the paths used in this guide. Change `route_state_socket` only if you changed `listen_socket` in `state.conf`.

## 8. Wire the module into Unbound

Copy `examples/unbound/dns-to-bgp.conf` to `/etc/unbound/unbound.conf.d/` (or merge it into `unbound.conf`):

```
server:
    module-config: "validator python iterator"
    chroot: ""
    prefetch: yes

python:
    python-script: "/opt/dns-to-bgp/unbound-bgp-announcer/announcer.py"
```

`chroot: ""` is mandatory: the Python module does not work under chroot. If your distribution confines Unbound with AppArmor or SELinux, allow it to read `/opt/dns-to-bgp/` and to write to `/run/dns-to-bgp/state.sock`. On Ubuntu, add to `/etc/apparmor.d/local/usr.sbin.unbound`:

```
/opt/dns-to-bgp/** r,
/run/dns-to-bgp/state.sock rw,
```

and run `sudo apparmor_parser -r /etc/apparmor.d/usr.sbin.unbound`. A denial shows up in `dmesg | grep -i denied`.

Check the configuration, but do not restart Unbound yet: `sudo unbound-checkconf`.

## 9. Configure BIRD

`examples/bird/dns-bgp.conf` is a starting point: a BGP protocol that connects to the daemon on port 1790 and imports host routes (`/32` and `/128`). Include it in your BIRD configuration, adapt the addresses and filters, then:

```bash
sudo birdc configure
```

The daemon announces `/32` and `/128` routes only. BIRD must accept the session as a normal BGP peer; the example uses iBGP (same AS on both sides). If the two addresses are not on a directly connected subnet, uncomment `multihop`.

## 10. Start

Start the daemon first, then Unbound:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now unbound-route-state
sudo systemctl restart unbound
sudo journalctl -u unbound-route-state -n 20 --no-pager
```

Expected in the log: `listening on /run/dns-to-bgp/state.sock`. Whether BIRD has connected is shown by `ctl bgp` (see below). A restart of Unbound is only needed when the module is first installed or its code changes; domain lists and rules are hot-reloaded.

## 11. Verify

```bash
CTL="sudo /opt/dns-to-bgp/unbound-route-state/unbound-route-state ctl"

$CTL bgp                          # session state with BIRD: Established
dig @127.0.0.1 www.cdn.example.net  # resolve a name that matches a pattern
$CTL routes                       # the answer's addresses are now live routes
$CTL route 203.0.113.10           # all attributes of one route
$CTL stats                        # counters
sudo birdc show route protocol dns_bgp_daemon
```

When the DNS TTL expires, the route is withdrawn automatically (unless you enabled the keepalive below).

## 12. Optional features

Both are off by default and described in the [reference](reference-route-state.md).

- **Keep routes while traffic flows** (`[keepalive] enabled = yes`): uses `conntrack -L` to keep a route announced after its DNS TTL has expired while connections to that IP are still open. Requires the `conntrack` package. The provided systemd unit already grants `CAP_NET_ADMIN`.
- **Flush Unbound's cache at start** (`[unbound] flush_on_start = yes`, and `ctl flush` at any time): Unbound serves repeated queries from its cache without calling the module, so after a daemon restart cached names do not appear as routes until their TTL expires. Flushing makes clients' next queries pass through the module again. Needs `remote-control` enabled in Unbound (included in the example snippet).

## 13. Updating

```bash
git pull && make build && sudo make install
sudo systemctl restart unbound-route-state
sudo systemctl restart unbound          # only if the Python files changed
```

Use a full `systemctl restart unbound` after changing the Python files. `unbound-control reload` may keep old copies of helper modules in memory; the module logs `RUNNING OLD CODE ... Restart Unbound` when it detects that.

## 14. Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Unbound logs `FAILED TO START ... next retry in ...` | The module could not read its config or domains directory. Fix the reason in the message (path, permissions, AppArmor); it retries by itself every 60 s, then hourly. |
| `dropped N event(s) ... Broken pipe` / no routes appear | The daemon is not running, or the socket path differs between `announcer.conf` and `state.conf`. |
| Routes are announced but BIRD does not show them | `as_path` in `communities.conf` contains BIRD's own ASN (BIRD silently drops the route; the daemon logs a WARN), or your BIRD import filter rejects them. |
| IPv6 routes are missing | `next_hop_v6` is empty in `state.conf`. |
| `events channel full` warning | Raise `event_queue_size` in `[limits]`. |
| BGP session takes minutes to come back after a restart | The delay is on the BIRD side (`error wait time`); see "BGP session recovery after a restart" in the [reference](reference-route-state.md). |
| Routes disappear after a daemon restart, then return slowly | Expected: the table starts empty. Use graceful restart on the BIRD side and `flush_on_start`. |
| `ctl` says permission denied | Run it with `sudo` or add your user to the `unbound` group. |
