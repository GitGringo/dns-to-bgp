"""
announcer.py - the Unbound Python module of dns-to-bgp.

For every DNS answer that Unbound is about to return, this module looks at the
A/AAAA records, checks the owner name against the configured domain patterns
(see domains.py / matcher.py) and, on a match, sends one event per RRset to the
unbound-route-state daemon over a UNIX socket (see transport.py). The answer
itself is never delayed, blocked or modified.

Hook contract (Unbound pythonmod):
  - init_standard(id, env) / deinit(id): lifecycle, called once each.
  - inform_super(id, qstate, superqstate, qdata): unused, always True.
  - operate(id, event, qstate, qdata): the only hook with real logic.
    MODULE_EVENT_NEW/PASS  -> hand over to the iterator (MODULE_WAIT_MODULE).
    MODULE_EVENT_MODDONE   -> qstate.return_msg holds the final answer; this
    is where extraction, matching and sending happen, followed by
    MODULE_FINISHED unconditionally.

MODULE_EVENT_*, MODULE_WAIT_MODULE, MODULE_FINISHED, RR_TYPE_A and RR_TYPE_AAAA
are injected into this module's global namespace by Unbound at load time. They
do not exist when the file is imported outside a running Unbound process, which
is why extract_events() is a standalone function operating on plain data.
"""

from __future__ import annotations

# Unbound's pythonmod loads this script through its C embedding API rather
# than `python3 announcer.py`, so the script's own directory is NOT added to
# sys.path automatically. Without the line below, the sibling imports
# (config, transport, ...) fail with ModuleNotFoundError.
import os
import sys
import socket
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import config as _config
import domains as _domains
import matcher as _matcher
import ratelimit as _ratelimit
import transport as _transport

_LOADED_AT = time.time()  # see _stale_modules()

_reloader = None  # domains.DomainReloader; its .matcher is the current trie
_transport_client = None
_cfg = None
_limiter = None  # RateLimiter for this module's own warnings; set on successful init

# Lazy re-initialisation: if init fails (config unreadable, domains directory
# missing, ...) the module stays disabled, but operate() retries - every
# _RETRY_FAST_S for the first _FAST_ATTEMPTS failures (the failed first start
# counts), then every _RETRY_SLOW_S.
_RETRY_FAST_S = 60.0
_RETRY_SLOW_S = 3600.0
_FAST_ATTEMPTS = 10
_init_failures = 0
_next_init_try = 0.0
_clock = time.monotonic  # injectable clock


def init_standard(id, env):  # noqa: A002 - "id" is Unbound's own parameter name
    return _try_init()


def _try_init() -> bool:
    global _reloader, _transport_client, _cfg, _limiter, _init_failures, _next_init_try
    try:
        cfg = _config.load_config()
        limiter = _ratelimit.RateLimiter(_log_warn, cfg.log_rate_limit_per_min)
        reloader = _domains.DomainReloader(
            cfg.domains_dir, cfg.max_patterns, cfg.reload_interval_s,
            warn=lambda key, msg: limiter.warn(key, msg), info=_log_info,
        )
        reloader.load_initial()
        client = _transport.Transport(
            cfg.socket_path,
            log=_log_warn,
            reconnect_backoff_s=cfg.reconnect_backoff_ms / 1000.0,
            log_rate_limit_per_min=cfg.log_rate_limit_per_min,
        )
    except Exception as exc:  # noqa: BLE001 - never raise into Unbound
        # Module-level state stays untouched (None), so operate() treats the
        # module as disabled and stays silent on each DNS answer; the only
        # log lines are these, at most one per retry interval.
        _init_failures += 1
        delay = _RETRY_FAST_S if _init_failures < _FAST_ATTEMPTS else _RETRY_SLOW_S
        _next_init_try = _clock() + delay
        _log_warn(
            f"dns-bgp-announcer: FAILED TO START (attempt {_init_failures}, "
            f"module disabled, next retry in {int(delay)}s): {exc}"
        )
        return False
    # Publish only after everything succeeded - never a half-initialised module.
    _cfg, _limiter, _reloader, _transport_client = cfg, limiter, reloader, client
    if _init_failures:
        _log_info(f"dns-bgp-announcer: started after {_init_failures} failed attempt(s)")
    _init_failures = 0
    stale = _stale_modules()
    if stale:
        _log_warn(
            "dns-bgp-announcer: RUNNING OLD CODE - " + ", ".join(stale) + " changed on disk after being "
            "loaded; this Unbound reload did not re-import it. Restart Unbound "
            "(systemctl restart unbound) to load the new code."
        )
    return True


def _stale_modules() -> list:
    """Names of this package's source files that changed on disk AFTER they were
    loaded into the running interpreter, i.e. Unbound is still executing old
    code. `unbound-control reload` / `reload_keep_cache` may keep already
    imported modules in memory; this makes that visible instead of silent.
    (A file replaced with `cp -p`/`rsync -t` keeps an old mtime and is not
    detected; plain `cp` / `install` / `mv` of a fresh file is.)"""
    stale = []
    here = globals()
    for name, mod_globals in (
        ("announcer.py", here),
        ("config.py", vars(_config)),
        ("domains.py", vars(_domains)),
        ("matcher.py", vars(_matcher)),
        ("ratelimit.py", vars(_ratelimit)),
        ("transport.py", vars(_transport)),
    ):
        loaded, path = mod_globals.get("_LOADED_AT"), mod_globals.get("__file__")
        try:
            if loaded is not None and path and os.path.getmtime(path) > loaded:
                stale.append(name)
        except OSError:
            pass
    return stale


def _ensure_ready() -> bool:
    if _reloader is not None and _transport_client is not None:
        return True
    if _clock() < _next_init_try:
        return False
    return _try_init()


def deinit(id):  # noqa: A002
    if _transport_client is not None:
        _transport_client.close()
    return True


def inform_super(id, qstate, superqstate, qdata):  # noqa: A002
    # Unused: this module never spawns sub-queries of its own.
    return True


def operate(id, event, qstate, qdata):  # noqa: A002
    if event in (MODULE_EVENT_NEW, MODULE_EVENT_PASS):  # noqa: F821 - injected by Unbound
        qstate.ext_state[id] = MODULE_WAIT_MODULE  # noqa: F821
        return True

    if event == MODULE_EVENT_MODDONE:  # noqa: F821
        try:
            _handle_answer(qstate)
        except Exception as exc:  # noqa: BLE001 - never let this hook crash Unbound
            _warn_limited("operate:" + type(exc).__name__,
                          f"dns-bgp-announcer: operate() error (answer still delivered): {exc}")
        qstate.ext_state[id] = MODULE_FINISHED  # noqa: F821
        return True

    qstate.ext_state[id] = MODULE_FINISHED  # noqa: F821
    return True


def _handle_answer(qstate) -> None:
    if not _ensure_ready():
        return  # init failed (logged per retry) - module disabled, answers pass untouched
    if qstate.return_msg is None or qstate.return_msg.rep is None:
        return
    _reloader.maybe_reload()
    for event in extract_events(qstate.return_msg.rep, _reloader.matcher):
        _transport_client.send(event)


# ---------------------------------------------------------------------
# extract_events: the one piece of real logic, kept independent of Unbound's
# injected globals so it works on plain data.
# ---------------------------------------------------------------------

def extract_events(rep, matcher) -> list[dict]:
    """Turn a reply (`qstate.return_msg.rep`) into a list of events.

    Only A and AAAA RRsets whose owner name matches `matcher` are reported.
    Each event is {"fqdn", "ttl", "type", "records"}. The exact layout of
    rr_data (with or without a 2-byte RDLENGTH prefix) differs between
    Unbound builds, so the rdata parsers below take the trailing 4/16 bytes,
    which is correct for both layouts.
    """
    events: list[dict] = []

    # Use the explicit RRset count and index the C array by it.
    count = getattr(rep, "rrset_count", 0)
    rrsets = getattr(rep, "rrsets", None)

    if not rrsets or count == 0:
        return events

    # Iterate strictly by count so we never run past the end of the C array.
    for i in range(count):
        rrset = rrsets[i]
        if rrset is None:
            continue

        rk = rrset.rk
        owner = rk.dname_str
        if not matcher.matches(owner):
            continue

        rr_type = _rr_type_name(rk)
        if rr_type is None:
            continue  # not A/AAAA: not our concern

        data = rrset.entry.data
        records = []
        for j in range(data.count):
            raw = bytes(data.rr_data[j])
            addr = _parse_a_rdata(raw) if rr_type == "A" else _parse_aaaa_rdata(raw)
            if addr is not None:
                records.append(addr)

        if not records:
            continue

        events.append({
            "fqdn": owner.rstrip("."),
            "ttl": int(data.ttl),
            "type": rr_type,
            "records": records,
        })
    return events


def _rr_type_name(rk) -> str | None:
    # rk.type is in network byte order; convert to host order.
    t = socket.ntohs(rk.type)

    if t == RR_TYPE_A:  # noqa: F821
        return "A"
    if t == RR_TYPE_AAAA:  # noqa: F821
        return "AAAA"
    return None


def _parse_a_rdata(raw: bytes) -> str | None:
    if len(raw) < 4:
        return None
    b = raw[-4:]
    return f"{b[0]}.{b[1]}.{b[2]}.{b[3]}"


def _parse_aaaa_rdata(raw: bytes) -> str | None:
    if len(raw) < 16:
        return None
    b = raw[-16:]
    return ":".join(f"{(b[i] << 8) | b[i + 1]:x}" for i in range(0, 16, 2))


def _warn_limited(key: str, msg: str) -> None:
    # Rate-limited (per key) warning; falls back to plain output if the
    # limiter isn't up yet (init_standard failed before creating it).
    if _limiter is None:
        _log_warn(msg)
    else:
        _limiter.warn(key, msg)


def _log_warn(msg: str) -> None:
    print(f"WARN: {msg}")


def _log_info(msg: str) -> None:
    print(f"INFO: {msg}")
