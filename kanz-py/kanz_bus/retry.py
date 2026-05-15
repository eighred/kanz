"""Bounded retry config + DLQ wire conventions (EVT-18d).

Mirrors the Go ``RetryConfig`` (EVT-17e). Retry runs inside the Consumer's
dispatch — it blocks the broker delivery for the duration of the sequence,
so long backoffs × large ``max_attempts`` trade per-partition throughput
for resilience.
"""

from __future__ import annotations

from dataclasses import dataclass

_DEFAULT_MAX_ATTEMPTS = 1
_DEFAULT_INITIAL_BACKOFF = 0.1
_DEFAULT_MAX_BACKOFF = 30.0


@dataclass
class RetryConfig:
    """Total attempts including the first; 0 or 1 means "no retry, surface
    or DLQ on first failure". Backoff doubles each attempt up to
    ``max_backoff_seconds``. No jitter at this layer (operational hardening
    is a follow-up)."""

    max_attempts: int = _DEFAULT_MAX_ATTEMPTS
    initial_backoff_seconds: float = _DEFAULT_INITIAL_BACKOFF
    max_backoff_seconds: float = _DEFAULT_MAX_BACKOFF

    def backoff(self, attempt: int) -> float:
        """Seconds to wait before the (attempt+1)-th try; ``attempt`` is 1-based."""
        if attempt < 1:
            return 0.0
        init = self.initial_backoff_seconds if self.initial_backoff_seconds > 0 else _DEFAULT_INITIAL_BACKOFF
        cap = self.max_backoff_seconds if self.max_backoff_seconds > 0 else _DEFAULT_MAX_BACKOFF
        d = init
        for _ in range(1, attempt):
            d *= 2
            if d >= cap:
                return cap
        return d


# DLQ subject convention: ``dlq.<original-subject>``. Matches the NATS
# ``dlq.>`` stream and per-topic ``dlq.{name}`` Kafka topics already
# provisioned in EVT-08/09.
DLQ_SUBJECT_PREFIX = "dlq."
