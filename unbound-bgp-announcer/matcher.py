"""
matcher.py - reversed-label trie for wildcard domain matching.

Why a trie and not a linear list or regex: with 1000+ patterns configured,
a linear scan on every DNS answer inside Unbound's resolution hot path would
be an O(n)-per-answer bottleneck. A trie keyed by domain label,
walked from the TLD inward, gives O(depth) lookup (depth = number of
labels in the query name, typically 3-6) regardless of how many patterns
are loaded - the number of patterns only affects trie *size*, never
lookup cost.

Two pattern forms:
  - "*.example.com"  - wildcard: matches any PROPER subdomain
                        (sub.example.com, a.b.example.com, ...) but
                        deliberately NEVER the apex (example.com) itself.
  - "example.com"     - exact: matches only the apex, never subdomains.
                        A wildcard pattern never implicitly grants an
                        apex match - that must be a separate, explicit
                        rule.

Matching is case-insensitive and tolerant of a trailing root dot
("example.com." == "example.com"), since both forms appear in real DNS
traffic depending on resolver/client behaviour.
"""

from __future__ import annotations

import time

# When this module was loaded into the interpreter. announcer.py compares it with the
# file's mtime to detect that Unbound is still running OLD code (see _stale_modules).
_LOADED_AT = time.time()


class _Node:
    __slots__ = ("children", "is_wildcard", "is_exact")

    def __init__(self):
        self.children: dict[str, "_Node"] = {}
        self.is_wildcard = False
        self.is_exact = False


def _labels_reversed(fqdn: str) -> list[str]:
    """"sub.example.com." -> ["com", "example", "sub"] - TLD first, so the
    trie is walked from the stable, high-fanout-shared end inward."""
    fqdn = fqdn.strip().rstrip(".").lower()
    if not fqdn:
        return []
    return fqdn.split(".")[::-1]


class WildcardTrie:
    """Holds every configured domain pattern. Load-time only mutates via
    add(); match-time (matches()) never mutates - safe to call from
    Unbound's operate() hook without any locking concern, since nothing
    else mutates a trie once it is published: hot-reload (domains.py)
    builds a brand-new trie and swaps the reference atomically, it never
    edits a live one.
    """

    def __init__(self):
        self._root = _Node()
        self.count = 0  # number of patterns added - for max_patterns enforcement

    def add(self, pattern: str) -> None:
        """Add one pattern, either "*.suffix" (wildcard) or "exact.form"
        (apex-only). Raises ValueError for anything else malformed (e.g.
        a bare "*", or "*.")."""
        pattern = pattern.strip().rstrip(".").lower()
        if not pattern:
            raise ValueError("empty pattern")

        if pattern.startswith("*."):
            suffix = pattern[2:]
            if not suffix:
                raise ValueError(f"malformed wildcard pattern: {pattern!r}")
            labels = suffix.split(".")[::-1]
            node = self._insert_path(labels)
            node.is_wildcard = True
        elif "*" in pattern:
            # Only the "*.suffix" form is supported - a "*" anywhere else
            # (mid-label, multiple wildcards, etc.) is refused rather than
            # silently doing something surprising with it.
            raise ValueError(f"unsupported wildcard position: {pattern!r}")
        else:
            labels = pattern.split(".")[::-1]
            node = self._insert_path(labels)
            node.is_exact = True

        self.count += 1

    def _insert_path(self, labels: list[str]) -> _Node:
        node = self._root
        for label in labels:
            node = node.children.setdefault(label, _Node())
        return node

    def matches(self, fqdn: str) -> bool:
        """True if fqdn matches any pattern added so far. See the module
        docstring for exactly what "matches" means for each pattern form.
        """
        labels = _labels_reversed(fqdn)
        if not labels:
            return False

        node = self._root
        for i, label in enumerate(labels):
            child = node.children.get(label)
            if child is None:
                return False
            node = child
            if node.is_wildcard and i < len(labels) - 1:
                return True
        return node.is_exact
