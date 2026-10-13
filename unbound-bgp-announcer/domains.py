"""
domains.py - loads domain patterns from a directory of *.list files and
hot-reloads them.

The directory is filled by external tools: any number of *.list files; hidden
files, temporary files (*.tmp, *~, *.swp) and anything without the .list
extension are ignored. All files are merged into ONE new WildcardTrie.

Hot reload (DomainReloader.maybe_reload, called from operate()):
  - at most once per interval_s (a cheap monotonic-clock comparison);
  - the directory fingerprint is the sorted list of (name, mtime_ns, size);
    if it is unchanged, nothing happens;
  - if it changed, a new trie is built from scratch. Invalid lines are skipped
    with a warning and the rest is loaded. If the directory or a file cannot be
    read, or max_patterns is exceeded, the whole reload is rejected and the
    previous trie stays in place (a partial set would silently shrink coverage);
  - on success, the .matcher reference is swapped with a single assignment.
Tools writing into the directory should write files atomically (temporary file
plus rename into the directory).
"""

from __future__ import annotations

import os
import re
import stat
import time
from typing import Callable

from matcher import WildcardTrie

# When this module was loaded into the interpreter. announcer.py compares it with the
# file's mtime to detect that Unbound is still running OLD code (see _stale_modules).
_LOADED_AT = time.time()


_LABEL_RE = re.compile(r"^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$")


def validate_pattern(pattern: str) -> str | None:
    """Return None if the pattern is syntactically valid, otherwise the reason.

    Valid forms: 'example.com' or '*.example.com' (ASCII / punycode)."""
    p = pattern.strip().rstrip(".").lower()
    if p.startswith("*."):
        p = p[2:]
    if not p:
        return "empty name"
    if len(p) > 253:
        return "name longer than 253 characters"
    for label in p.split("."):
        if not _LABEL_RE.match(label):
            return f"invalid label {label!r}"
    return None


def scan_dir(path: str) -> list[tuple[str, int, int]]:
    """Return the directory fingerprint. Raises OSError if it is unreadable."""
    out = []
    with os.scandir(path) as it:
        for e in it:
            if not e.name.endswith(".list") or e.name.startswith("."):
                continue
            st = e.stat()  # follows symlinks
            if not stat.S_ISREG(st.st_mode):
                continue
            out.append((e.name, st.st_mtime_ns, st.st_size))
    out.sort()
    return out


def load_dir(path: str, max_patterns: int, fingerprint,
             warn: Callable[[str, str], None]) -> WildcardTrie:
    """Build a WildcardTrie from the files listed in `fingerprint`."""
    trie = WildcardTrie()
    for name, _, _ in fingerprint:
        fpath = os.path.join(path, name)
        with open(fpath, "r", encoding="utf-8") as fh:
            for lineno, raw in enumerate(fh, start=1):
                line = raw.split("#", 1)[0].strip()
                if not line:
                    continue
                reason = validate_pattern(line)
                if reason is None:
                    try:
                        if trie.count >= max_patterns:
                            raise RuntimeError(
                                f"{path}: exceeds max_patterns={max_patterns} "
                                f"(at {name}:{lineno}) - refusing partial load"
                            )
                        trie.add(line)
                        continue
                    except ValueError as exc:
                        reason = str(exc)
                warn("domains:invalid-line",
                     f"dns-bgp-announcer: {name}:{lineno}: ignoring invalid pattern {line!r}: {reason}")
    return trie


class DomainReloader:
    """Holds the current matcher and swaps it when the directory changes."""

    def __init__(self, path: str, max_patterns: int, interval_s: float,
                 warn: Callable[[str, str], None], info: Callable[[str], None],
                 clock=time.monotonic):
        self.path = path
        self.max_patterns = max_patterns
        self.interval_s = interval_s
        self._warn = warn
        self._info = info
        self._clock = clock
        self._fp = None
        self._next_check = 0.0
        self.matcher = WildcardTrie()

    def load_initial(self) -> None:
        """Initial load; errors propagate so that startup fails loudly."""
        fp = scan_dir(self.path)
        self.matcher = load_dir(self.path, self.max_patterns, fp, self._warn)
        self._fp = fp
        self._next_check = self._clock() + self.interval_s
        if not fp:
            self._warn("domains:empty", f"dns-bgp-announcer: no *.list files in {self.path}")
        self._info(f"dns-bgp-announcer: loaded {self.matcher.count} pattern(s) from {len(fp)} file(s) in {self.path}")

    def maybe_reload(self) -> None:
        """Re-scan the directory if the interval has elapsed; never raises."""
        if self.interval_s <= 0:
            return
        now = self._clock()
        if now < self._next_check:
            return
        self._next_check = now + self.interval_s
        try:
            fp = scan_dir(self.path)
            if fp == self._fp:
                return
            new = load_dir(self.path, self.max_patterns, fp, self._warn)
        except Exception as exc:  # noqa: BLE001 - reload must never break DNS; keep old trie
            self._warn("domains:reload-failed", f"dns-bgp-announcer: domains reload failed, keeping previous set: {exc}")
            return
        self.matcher = new  # single reference assignment - readers see old or new, never partial
        self._fp = fp
        self._info(f"dns-bgp-announcer: reloaded {new.count} pattern(s) from {len(fp)} file(s) in {self.path}")
