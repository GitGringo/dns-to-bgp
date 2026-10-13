# unbound-bgp-announcer reference

`unbound-bgp-announcer` is a Python module for Unbound. It matches DNS answers against wildcard patterns and sends NDJSON events to `unbound-route-state` over a UNIX socket. It is the only integration point with Unbound and contains no other logic.

## Files

- `matcher.py`: wildcard matching on reversed labels (trie), O(depth) per query.
- `transport.py`: non-blocking UNIX-socket client; never blocks and never raises.
- `config.py`: loads `announcer.conf`.
- `domains.py`: loads patterns from the `domains.d/*.list` directory, with hot reload.
- `ratelimit.py`: log rate limiter (per message kind, `log_rate_limit_per_min`).
- `announcer.py`: the Unbound hooks (`init_standard` / `deinit` / `operate` / `inform_super`).

## Configuration: announcer.conf

Default path: `/opt/dns-to-bgp/conf.d/announcer.conf`. It can be overridden with the environment variable `DNS_BGP_ANNOUNCER_CONFIG`, set for the Unbound process (for example via `systemctl edit unbound` and `Environment=`).

```ini
[general]
route_state_socket     = /run/dns-to-bgp/state.sock
connect_timeout_ms     = 50
send_timeout_ms        = 20
reconnect_backoff_ms   = 500
log_rate_limit_per_min = 60

[domains]
domains_dir            = /opt/dns-to-bgp/domains.d
reload_interval_s      = 5
max_patterns           = 5000
```

| Key | Example value | Description |
|---|---|---|
| `route_state_socket` | `/run/dns-to-bgp/state.sock` | UNIX socket of `unbound-route-state` (its `listen_socket`). |
| `connect_timeout_ms` | `50` | Connect timeout. |
| `send_timeout_ms` | `20` | Send timeout. |
| `reconnect_backoff_ms` | `500` | Minimum interval between reconnect attempts after a failure. |
| `log_rate_limit_per_min` | `60` | Log rate limit per message kind. |
| `domains_dir` | `/opt/dns-to-bgp/domains.d` | Directory with `*.list` files. |
| `reload_interval_s` | `5` | Domain directory check period; `0` disables hot reload. |
| `max_patterns` | `5000` | Upper bound on the number of patterns. |

## Unbound integration

In `unbound.conf`:

```
server:
    module-config: "validator python iterator"
    chroot: ""              # pythonmod does not work under chroot
    prefetch: yes

python:
    python-script: "/opt/dns-to-bgp/unbound-bgp-announcer/announcer.py"
```

`chroot: ""` is a requirement, not an option.

Unbound injects names such as `MODULE_EVENT_NEW` and `RR_TYPE_A` into the module's namespace when it loads it. The event extraction logic is kept in a separate function, `extract_events()`, apart from the hooks themselves.

### rr_data layout

The exact byte layout of `rr_data` (whether a 2-byte RDLENGTH prefix precedes the address) is handled defensively: `_parse_a_rdata` and `_parse_aaaa_rdata` in `announcer.py` take the last N bytes rather than the first, so both variants work.

## Deployment layout

```
/opt/dns-to-bgp/
    unbound-bgp-announcer/
        announcer.py config.py domains.py matcher.py ratelimit.py transport.py
    conf.d/
        announcer.conf            # from examples/conf.d/announcer.conf
    domains.d/
        *.list                    # filled externally
```

Permissions (the module runs inside Unbound as the `unbound` user and only needs read access):

| Path | Owner | Mode |
|---|---|---|
| `/opt/dns-to-bgp`, `.../unbound-bgp-announcer`, `.../conf.d` | `root:root` | `0755` |
| `*.py` | `root:root` | `0644` |
| `conf.d/announcer.conf` | `root:unbound` | `0640` |
| `/opt/dns-to-bgp/domains.d/` | `root:unbound` | `0755` (if a user other than root writes there, use that writer's owner/group; `unbound` must be able to read) |
| `domains.d/*.list` (same directory) | `root:unbound` | `0640` or `0644` |

If Unbound runs under AppArmor or SELinux on the host, make sure the profile allows reading `/opt/dns-to-bgp/`; otherwise the module will not load (check `dmesg | grep -i denied`).

## Start-up failure and re-initialisation

If `init_standard()` fails (missing or unreadable config, missing domains directory, and so on), the module disables itself: DNS answers are served as usual, but nothing is announced. The log gets `FAILED TO START (attempt N, ... next retry in Ns): <reason>`. After that, `operate()` lazily retries initialisation: the first 10 failures (including the first) once every 60 s, then once every 3600 s. Success is logged (`started after N failed attempt(s)`); retries between intervals write nothing. Nothing needs to be restarted.

## Updating the module code: restart or reload

`systemctl restart unbound` is guaranteed to load the new code. With `unbound-control reload` / `reload_keep_cache` it is not: Unbound re-initialises the module (`init_standard` is called again, the config and domains are re-read), but helper files that were already imported (`transport.py`, `domains.py`, and so on) may stay in memory in their old version.

To keep this from going unnoticed, on every initialisation the module compares the mtime of its files with the time they were loaded and, if a file changed later, logs one `WARN`: `RUNNING OLD CODE - transport.py ...  Restart Unbound`. If there is no such line after a reload, the new code really is loaded. (A file copied with its old mtime preserved, such as `cp -p` or `rsync -t`, is not detected this way; plain `cp`, `install` and `mv` are.)

## Daemon restart and the first event

The Python module keeps a connection to the `unbound-route-state` socket. After `systemctl restart unbound-route-state` that connection is dead, but the module only finds out at the next send. Previously the event that triggered it (the first DNS answer after the daemon restart) was silently lost (`dropped 1 event(s) ... Broken pipe` in the Unbound log). Now, when a send fails on a previously established connection, the module reconnects once immediately and repeats the send (no bytes of the event were written, so NDJSON framing is not affected). If the daemon is really down, the event is dropped as before, and further connection attempts follow `reconnect_backoff_ms`.

## Domain list: directory and hot reload

Patterns are read from `domains_dir` (default `/opt/dns-to-bgp/domains.d`): all `*.list` files (hidden files and other extensions are ignored), merged into one trie. Changes are picked up **without restarting Unbound**: every `reload_interval_s` (default 5 s, `0` = off), `operate()` compares a fingerprint of the directory (name, mtime and size of each file).

- Invalid lines (FQDN syntax) are skipped with a warning; the rest are loaded.
- If the directory or a file is unreadable, or `max_patterns` is exceeded, the whole reload is rejected and the previous set stays; it is retried at the next check.
- An empty directory is allowed (with a warning): if an external source clears the directory, routes stop being announced.
- **Write files atomically:** write to a temporary file in the same directory **without** the `.list` extension (or with a hidden name), then `mv`/`rename` it to `NAME.list`. Otherwise a half-written file may be read.
- Several Unbound workers (`num-threads`) re-read the directory independently.

```bash
printf '*.example.com\n' > domains.d/.new.tmp && mv domains.d/.new.tmp domains.d/new.list
```
