from __future__ import annotations

import time

from kanz_bus import DedupWindow


def test_dedup_seen_after_record():
    w = DedupWindow()
    assert not w.seen("k1")
    w.record("k1")
    assert w.seen("k1")
    assert not w.seen("k2")


def test_dedup_empty_key_ignored():
    w = DedupWindow()
    assert not w.seen("")
    w.record("")  # no-op
    assert not w.seen("")


def test_dedup_ttl_expiry():
    w = DedupWindow(ttl_seconds=0.05, max_entries=100)
    w.record("k")
    assert w.seen("k")
    time.sleep(0.08)
    assert not w.seen("k")


def test_dedup_capacity_eviction():
    w = DedupWindow(ttl_seconds=60.0, max_entries=3)
    w.record("a")
    time.sleep(0.001)
    w.record("b")
    time.sleep(0.001)
    w.record("c")
    time.sleep(0.001)
    w.record("d")  # evicts "a"
    assert not w.seen("a")
    for k in ("b", "c", "d"):
        assert w.seen(k), f"{k} should still be in window"


def test_dedup_disabled_modes():
    for kwargs in (
        {"ttl_seconds": 0},
        {"max_entries": 0},
        {"ttl_seconds": 0, "max_entries": 0},
    ):
        w = DedupWindow(**kwargs)
        assert w.disabled
        assert not w.seen("k")
        w.record("k")  # no-op
        assert not w.seen("k")
