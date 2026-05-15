from __future__ import annotations

import pytest

from kanz_bus import RetryConfig


def test_backoff_exponential_with_cap():
    cfg = RetryConfig(
        max_attempts=5,
        initial_backoff_seconds=0.010,
        max_backoff_seconds=0.100,
    )
    # attempts: 10, 20, 40, 80, cap at 100ms (160ms uncapped)
    expected = [0.010, 0.020, 0.040, 0.080, 0.100, 0.100]
    for attempt, want in enumerate(expected, start=1):
        assert cfg.backoff(attempt) == pytest.approx(want)


def test_backoff_zero_or_negative_attempt():
    cfg = RetryConfig(initial_backoff_seconds=0.010, max_backoff_seconds=1.0)
    assert cfg.backoff(0) == 0.0
    assert cfg.backoff(-3) == 0.0


def test_backoff_defaults_on_zero_fields():
    # Zero-value config still produces a sensible non-zero backoff via the
    # default fallback (100ms initial / 30s cap).
    assert RetryConfig().backoff(1) == pytest.approx(0.1)
