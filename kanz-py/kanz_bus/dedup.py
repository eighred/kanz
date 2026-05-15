"""Consumer-side dedup window (EVT-18d).

Mirrors the Go ``DedupWindow`` (kanz/pkg/bus/dedup.go, EVT-17d). Per-instance
in-memory only — cross-pod dedup is out of scope; the canonical defense
against cross-instance redelivery is idempotent handlers
(event-class-rules §1). The default TTL matches NATS JetStream's broker-side
dedup window (2 min) so the two layers reinforce.
"""

from __future__ import annotations

import time

_DEFAULT_TTL_SECONDS = 120.0
_DEFAULT_MAX_ENTRIES = 10_000


class DedupWindow:
    """Sliding window keyed on idempotency_key.

    Split ``seen`` / ``record`` API lets :class:`Consumer` record only after
    a *successful* dispatch — a transient failure must remain retry-able.
    Two concurrent deliveries of the same key can both pass ``seen()`` before
    either ``record()``s, so handlers must be idempotent
    (event-class-rules §1) — this window is a perf optimization, not a
    correctness primitive.

    ``ttl_seconds <= 0`` or ``max_entries <= 0`` puts the window into a
    disabled mode where ``seen`` always returns ``False`` and ``record`` is
    a no-op (matches the nil-receiver pattern in the Go client).
    """

    def __init__(
        self,
        ttl_seconds: float = _DEFAULT_TTL_SECONDS,
        max_entries: int = _DEFAULT_MAX_ENTRIES,
    ) -> None:
        self._disabled = ttl_seconds <= 0 or max_entries <= 0
        self._ttl = ttl_seconds
        self._max = max_entries
        self._seen: dict[str, float] = {}

    @property
    def disabled(self) -> bool:
        return self._disabled

    def seen(self, key: str) -> bool:
        if self._disabled or not key:
            return False
        t = self._seen.get(key)
        if t is None:
            return False
        return (time.monotonic() - t) < self._ttl

    def record(self, key: str) -> None:
        if self._disabled or not key:
            return
        now = time.monotonic()
        if len(self._seen) >= self._max:
            self._gc(now)
        self._seen[key] = now

    def _gc(self, now: float) -> None:
        # Sweep expired entries first; if still at capacity, evict the oldest.
        for k in [k for k, t in self._seen.items() if (now - t) >= self._ttl]:
            del self._seen[k]
        while len(self._seen) >= self._max:
            oldest_k = min(self._seen, key=self._seen.__getitem__)
            del self._seen[oldest_k]
