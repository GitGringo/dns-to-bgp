"""
config.py - loads announcer.conf (default /opt/dns-to-bgp/conf.d/announcer.conf).
"""

from __future__ import annotations

import configparser
import os
import time
from dataclasses import dataclass

# When this module was loaded into the interpreter. announcer.py compares it with the
# file's mtime to detect that Unbound is still running OLD code (see _stale_modules).
_LOADED_AT = time.time()

# The config path can be overridden with the DNS_BGP_ANNOUNCER_CONFIG
# environment variable (set it in Unbound's environment if the file lives
# elsewhere).
DEFAULT_CONFIG_PATH = os.environ.get(
    "DNS_BGP_ANNOUNCER_CONFIG", "/opt/dns-to-bgp/conf.d/announcer.conf"
)


@dataclass
class Config:
    socket_path: str
    connect_timeout_ms: int
    send_timeout_ms: int
    reconnect_backoff_ms: int
    log_rate_limit_per_min: int
    domains_dir: str
    reload_interval_s: float
    max_patterns: int


def load_config(path: str = DEFAULT_CONFIG_PATH) -> Config:
    cfg = configparser.ConfigParser(inline_comment_prefixes=(";", "#"))
    if not cfg.read(path):
        raise RuntimeError(f"cannot read config file: {path}")

    g = cfg["general"]
    d = cfg["domains"]

    return Config(
        socket_path=g.get("route_state_socket", fallback="/run/bird/dns-bgp/state.sock"),
        connect_timeout_ms=g.getint("connect_timeout_ms", fallback=50),
        send_timeout_ms=g.getint("send_timeout_ms", fallback=20),
        reconnect_backoff_ms=g.getint("reconnect_backoff_ms", fallback=500),
        log_rate_limit_per_min=g.getint("log_rate_limit_per_min", fallback=60),
        domains_dir=d.get("domains_dir", fallback="/opt/dns-to-bgp/domains.d"),
        reload_interval_s=d.getfloat("reload_interval_s", fallback=5.0),
        max_patterns=d.getint("max_patterns", fallback=5000),
    )

