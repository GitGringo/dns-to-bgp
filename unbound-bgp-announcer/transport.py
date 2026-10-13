"""
transport.py - non-blocking UNIX socket client to unbound-route-state.

This is the most safety-critical file of the module: it runs inside Unbound's
own process and is called from operate() for every tracked DNS answer.
Anything here that blocks or raises would stall DNS resolution for every
client of the resolver, not just the DNS-to-BGP feature. Every method is
therefore built around one rule: send() never blocks and never raises. If the
event cannot be delivered it is dropped (and counted); the only retry is a
single resend on a fresh connection when the existing one turns out to be dead
(for example after the daemon restarted).

Wire format: one JSON object per line (NDJSON).
"""

from __future__ import annotations

import json
import socket
import time


# When this module was loaded into the interpreter. announcer.py compares it with the
# file's mtime to detect that Unbound is still running OLD code (see _stale_modules).
_LOADED_AT = time.time()

# MSG_NOSIGNAL: a send to a closed peer must raise EPIPE, never deliver SIGPIPE
# to the host process (Unbound). Linux has it; fall back to 0 elsewhere.
_SEND_FLAGS = getattr(socket, "MSG_NOSIGNAL", 0)


class Transport:
    """Non-blocking sender of NDJSON events to a UNIX stream socket."""

    def __init__(
        self,
        socket_path: str,
        log=None,
        reconnect_backoff_s: float = 0.5,
        log_rate_limit_per_min: int = 60,
    ):
        self._path = socket_path
        self._sock: socket.socket | None = None
        self._log = log or (lambda msg: None)
        self._reconnect_backoff_s = reconnect_backoff_s
        # -inf, not 0.0: with 0.0 the very first connect attempt was suppressed whenever
        # time.monotonic() (host uptime) was smaller than the backoff.
        self._last_connect_attempt = float("-inf")

        # Rate-limited logging: drops must be visible in aggregate rather than
        # spamming syslog once per dropped event.
        self._log_interval_s = 60.0 / max(log_rate_limit_per_min, 1)
        self._last_log_time = 0.0
        self._dropped_since_log = 0

    def send(self, obj: dict) -> bool:
        """Encode obj as one NDJSON line and attempt to send it.

        Returns True if the line was handed to the kernel socket buffer
        in full, False if dropped for any reason (not connected, buffer
        full, connection error). Never raises, never blocks - safe to
        call unconditionally from operate().
        """
        line = (json.dumps(obj, separators=(",", ":")) + "\n").encode("utf-8")

        # At most two attempts. The second happens only right after the
        # first hit a dead connection (OSError): that is what a restart of the
        # Go daemon looks like from here - this process still holds the old,
        # now closed socket and only learns so on the next send. Dropping that
        # one event (as an earlier version did) lost exactly the first DNS
        # answer after every daemon restart. Nothing was written when send()
        # raised, so resending once on a fresh connection cannot corrupt
        # the NDJSON framing. The retry's reconnect ignores the backoff window
        # once; if the daemon is really down it fails, and the normal backoff
        # applies to everything after it.
        for attempt in (0, 1):
            if not self._ensure_connected(force=attempt == 1):
                self._note_drop("not connected")
                return False

            try:
                sent = self._sock.send(line, _SEND_FLAGS)
            except BlockingIOError:
                # Kernel send buffer is full - the receiver (Go daemon) isn't
                # draining fast enough, or isn't running. Drop this one event
                # and move on; do not wait, do not retry.
                self._note_drop("send buffer full")
                return False
            except OSError as exc:
                # Broken pipe, connection reset, etc. - the connection is dead.
                self._close()
                if attempt == 0:
                    continue  # one immediate retry on a fresh connection
                self._note_drop(f"connection error: {exc}")
                return False
            break

        if sent < len(line):
            # A *partial* write on a stream socket is worse than a full
            # drop: the remaining bytes of this JSON line would be
            # silently prefixed onto whatever gets sent next, corrupting
            # NDJSON framing for the receiver from here on. Rather than
            # attempt an in-place resend (which risks the same partial-
            # write problem again under sustained backpressure), close
            # the connection outright - the receiver already treats a
            # torn stream as "drop the whole connection, don't try to
            # resynchronise" (internal/route-state's transport.go, same
            # discipline on the other end). A fresh connection starts
            # both sides on a clean line boundary.
            self._close()
            self._note_drop("partial write, framing would be corrupted")
            return False

        return True

    def close(self) -> None:
        """Called from deinit() on module unload/Unbound shutdown."""
        self._close()

    # -- internals -----------------------------------------------------

    def _ensure_connected(self, force: bool = False) -> bool:
        if self._sock is not None:
            return True

        now = time.monotonic()
        if not force and now - self._last_connect_attempt < self._reconnect_backoff_s:
            return False  # still within backoff window since the last failed attempt
        self._last_connect_attempt = now

        try:
            s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            s.setblocking(False)
            s.connect(self._path)
            self._sock = s
            return True
        except BlockingIOError:
            # Non-blocking connect() to a UNIX socket essentially always
            # either succeeds immediately or fails immediately (unlike
            # TCP's real handshake latency) - but handle the
            # in-progress case defensively rather than assume.
            self._sock = s
            return False
        except OSError as exc:
            # connect() itself failed (no listener, permission denied,
            # etc.) - s was created but never became self._sock, so it
            # must be closed explicitly here or the fd leaks silently.
            s.close()
            self._note_drop(f"connect failed: {exc}")
            return False

    def _close(self) -> None:
        if self._sock is not None:
            try:
                self._sock.close()
            except OSError:
                pass
            self._sock = None

    def _note_drop(self, reason: str) -> None:
        self._dropped_since_log += 1
        now = time.monotonic()
        if now - self._last_log_time < self._log_interval_s:
            return
        self._log(f"dns-bgp-announcer: dropped {self._dropped_since_log} event(s) "
                   f"since last log (most recent reason: {reason})")
        self._dropped_since_log = 0
        self._last_log_time = now
