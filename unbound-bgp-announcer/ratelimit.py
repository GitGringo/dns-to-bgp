"""
ratelimit.py - per-key log rate limiter for announcer.py.

One key is one kind of message: different kinds of warnings do not suppress
each other. For each key a message is let through at most once per `interval`
seconds (60 / log_rate_limit_per_min); suppressed repeats are counted and
appended to the next message let through for the same key. The limiter is
driven by calls only (no timers or threads), so a trailing batch of suppressed
messages is not printed until another call arrives.

per_min <= 0 disables limiting.
"""

from __future__ import annotations

import time
from typing import Callable

# When this module was loaded into the interpreter. announcer.py compares it with the
# file's mtime to detect that Unbound is still running OLD code (see _stale_modules).
_LOADED_AT = time.time()

_MAX_KEYS = 256  # bounds memory if keys ever turn out to be dynamic


class RateLimiter:
    """Calls `emit(msg)` for warnings, subject to the per-key rate limit."""

    def __init__(self, emit: Callable[[str], None], per_min: int, clock=time.monotonic):
        self._emit = emit
        self._interval = 60.0 / per_min if per_min > 0 else 0.0
        self._clock = clock
        self._last: dict[str, float] = {}
        self._suppressed: dict[str, int] = {}

    def warn(self, key: str, msg: str) -> None:
        if self._interval <= 0:
            self._emit(msg)
            return
        now = self._clock()
        last = self._last.get(key)
        if last is not None and now - last < self._interval:
            self._suppressed[key] = self._suppressed.get(key, 0) + 1
            return
        if last is None and len(self._last) >= _MAX_KEYS:
            self._last.clear()
            self._suppressed.clear()
        n = self._suppressed.pop(key, 0)
        self._last[key] = now
        if n:
            msg = f"{msg} [+{n} similar message(s) suppressed]"
        self._emit(msg)
