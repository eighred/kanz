"""PRED-11 feature-store boundary tests."""

from __future__ import annotations

import pytest
from google.protobuf.timestamp_pb2 import Timestamp

from inference.v1.feature_vector_pb2 import FeatureValue, FeatureVector

from kanz_inference.featurestore import FeatureStore, InMemoryFeatureStore


def _ts(seconds: int, nanos: int = 0) -> Timestamp:
    return Timestamp(seconds=seconds, nanos=nanos)


def _fv(
    subject_id: str = "AAPL",
    feature_set_ref: str = "equity-momentum:7",
    *,
    as_of: int = 1000,
    scalar: float = 1.5,
) -> FeatureVector:
    fv = FeatureVector(
        subject_id=subject_id,
        feature_set_ref=feature_set_ref,
        as_of=_ts(as_of),
    )
    fv.values["px"].scalar = scalar
    return fv


# --- Protocol compliance ----------------------------------------------


def test_in_memory_satisfies_protocol():
    # FeatureStore is not @runtime_checkable; assert structurally that
    # the trivial impl exposes the boundary surface.
    store = InMemoryFeatureStore()
    assert hasattr(store, "get") and hasattr(store, "put")
    _: FeatureStore = store  # static-shape assertion


# --- get / put round-trip ---------------------------------------------


async def test_put_then_get_latest_round_trip():
    store = InMemoryFeatureStore()
    await store.put(_fv(scalar=2.5))
    got = await store.get("AAPL", "equity-momentum:7")
    assert got is not None
    assert got.subject_id == "AAPL"
    assert got.values["px"].scalar == 2.5


async def test_get_miss_returns_none():
    store = InMemoryFeatureStore()
    assert await store.get("AAPL", "equity-momentum:7") is None


async def test_get_unknown_feature_set_returns_none():
    store = InMemoryFeatureStore()
    await store.put(_fv())
    # Same subject, different feature set — keyed jointly, so a miss.
    assert await store.get("AAPL", "credit-risk:3") is None


# --- Point-in-time semantics (the load-bearing property) --------------


async def test_get_as_of_returns_effective_vector_not_latest():
    store = InMemoryFeatureStore()
    await store.put(_fv(as_of=1000, scalar=1.0))
    await store.put(_fv(as_of=2000, scalar=2.0))
    await store.put(_fv(as_of=3000, scalar=3.0))

    # As-of exactly between the 2000 and 3000 marks → must see 2000's
    # value, never the later 3000 (that would be future leakage).
    got = await store.get("AAPL", "equity-momentum:7", _ts(2500))
    assert got is not None
    assert got.values["px"].scalar == 2.0


async def test_get_as_of_exact_match_inclusive():
    store = InMemoryFeatureStore()
    await store.put(_fv(as_of=1000, scalar=1.0))
    await store.put(_fv(as_of=2000, scalar=2.0))
    # as_of == a stored mark is inclusive (at-or-before).
    got = await store.get("AAPL", "equity-momentum:7", _ts(2000))
    assert got is not None
    assert got.values["px"].scalar == 2.0


async def test_get_as_of_before_all_returns_none():
    store = InMemoryFeatureStore()
    await store.put(_fv(as_of=2000, scalar=2.0))
    # Every stored vector is strictly newer than the requested time —
    # no point-in-time answer rather than leaking the future value.
    assert await store.get("AAPL", "equity-momentum:7", _ts(1000)) is None


async def test_get_as_of_nanos_ordering():
    store = InMemoryFeatureStore()
    await store.put(_fv(as_of=1000, scalar=1.0))  # nanos=0
    fv_late = _fv(scalar=9.0)
    fv_late.as_of.CopyFrom(_ts(1000, nanos=500))
    await store.put(fv_late)
    # Same seconds, larger nanos is later — as-of at nanos=400 sees the
    # earlier one, proving (seconds, nanos) ordering not seconds-only.
    got = await store.get("AAPL", "equity-momentum:7", _ts(1000, nanos=400))
    assert got is not None
    assert got.values["px"].scalar == 1.0


async def test_out_of_order_put_still_point_in_time_correct():
    store = InMemoryFeatureStore()
    # Insert newest-first; history must still resolve correctly.
    await store.put(_fv(as_of=3000, scalar=3.0))
    await store.put(_fv(as_of=1000, scalar=1.0))
    await store.put(_fv(as_of=2000, scalar=2.0))
    got = await store.get("AAPL", "equity-momentum:7", _ts(2000))
    assert got is not None
    assert got.values["px"].scalar == 2.0
    latest = await store.get("AAPL", "equity-momentum:7")
    assert latest is not None
    assert latest.values["px"].scalar == 3.0


async def test_put_same_as_of_overwrites():
    store = InMemoryFeatureStore()
    await store.put(_fv(as_of=1000, scalar=1.0))
    await store.put(_fv(as_of=1000, scalar=9.9))
    got = await store.get("AAPL", "equity-momentum:7", _ts(1000))
    assert got is not None
    assert got.values["px"].scalar == 9.9
    # Overwrite, not append: a single point in time, one value.
    assert await store.get("AAPL", "equity-momentum:7", _ts(2000)) is not None


# --- Subject / feature-set isolation ----------------------------------


async def test_distinct_subjects_isolated():
    store = InMemoryFeatureStore()
    await store.put(_fv(subject_id="AAPL", scalar=1.0))
    await store.put(_fv(subject_id="MSFT", scalar=2.0))
    a = await store.get("AAPL", "equity-momentum:7")
    m = await store.get("MSFT", "equity-momentum:7")
    assert a is not None and a.values["px"].scalar == 1.0
    assert m is not None and m.values["px"].scalar == 2.0


async def test_same_subject_distinct_feature_sets_isolated():
    store = InMemoryFeatureStore()
    await store.put(_fv(feature_set_ref="equity-momentum:7", scalar=1.0))
    await store.put(_fv(feature_set_ref="credit-risk:3", scalar=2.0))
    mom = await store.get("AAPL", "equity-momentum:7")
    cr = await store.get("AAPL", "credit-risk:3")
    assert mom is not None and mom.values["px"].scalar == 1.0
    assert cr is not None and cr.values["px"].scalar == 2.0


# --- Defensive copy ----------------------------------------------------


async def test_put_defensive_copies_input():
    store = InMemoryFeatureStore()
    fv = _fv(scalar=1.0)
    await store.put(fv)
    # Mutating the caller's message after put must not change stored state.
    fv.values["px"].scalar = 99.0
    got = await store.get("AAPL", "equity-momentum:7")
    assert got is not None
    assert got.values["px"].scalar == 1.0


async def test_get_returns_copy_not_live_entry():
    store = InMemoryFeatureStore()
    await store.put(_fv(scalar=1.0))
    got = await store.get("AAPL", "equity-momentum:7")
    assert got is not None
    got.values["px"].scalar = 42.0
    # Mutating the returned vector must not corrupt the store.
    again = await store.get("AAPL", "equity-momentum:7")
    assert again is not None
    assert again.values["px"].scalar == 1.0


# --- Validation --------------------------------------------------------


async def test_put_rejects_missing_subject_id():
    store = InMemoryFeatureStore()
    fv = _fv()
    fv.subject_id = ""
    with pytest.raises(ValueError, match="subject_id"):
        await store.put(fv)


async def test_put_rejects_missing_feature_set_ref():
    store = InMemoryFeatureStore()
    fv = _fv()
    fv.feature_set_ref = ""
    with pytest.raises(ValueError, match="feature_set_ref"):
        await store.put(fv)


async def test_put_rejects_missing_as_of():
    store = InMemoryFeatureStore()
    fv = FeatureVector(subject_id="AAPL", feature_set_ref="equity-momentum:7")
    with pytest.raises(ValueError, match="as_of"):
        await store.put(fv)
