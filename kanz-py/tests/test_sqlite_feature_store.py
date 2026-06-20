"""MLOPS-01f durable feature-store tests — the same PRED-11 point-in-time
contract as the in-memory impl, plus durability across restart."""

from __future__ import annotations

import pytest
from google.protobuf.timestamp_pb2 import Timestamp

from inference.v1.feature_vector_pb2 import FeatureVector

from kanz_inference.featurestore import FeatureStore, SqliteFeatureStore


def _ts(seconds: int, nanos: int = 0) -> Timestamp:
    return Timestamp(seconds=seconds, nanos=nanos)


def _fv(
    subject_id: str = "AAPL",
    feature_set_ref: str = "equity-momentum:7",
    *,
    as_of: int = 1000,
    scalar: float = 1.5,
) -> FeatureVector:
    fv = FeatureVector(subject_id=subject_id, feature_set_ref=feature_set_ref, as_of=_ts(as_of))
    fv.values["px"].scalar = scalar
    return fv


@pytest.fixture
def store():
    s = SqliteFeatureStore()  # :memory:
    yield s
    s.close()


# --- Protocol + round-trip --------------------------------------------


def test_satisfies_protocol(store):
    assert hasattr(store, "get") and hasattr(store, "put")
    _: FeatureStore = store  # static-shape assertion


async def test_put_then_get_latest_round_trip(store):
    await store.put(_fv(scalar=2.5))
    got = await store.get("AAPL", "equity-momentum:7")
    assert got is not None
    assert got.subject_id == "AAPL"
    assert got.values["px"].scalar == 2.5


async def test_get_miss_returns_none(store):
    assert await store.get("AAPL", "equity-momentum:7") is None


# --- Point-in-time semantics (the load-bearing property) --------------


async def test_get_as_of_returns_effective_not_latest(store):
    await store.put(_fv(as_of=1000, scalar=1.0))
    await store.put(_fv(as_of=2000, scalar=2.0))
    await store.put(_fv(as_of=3000, scalar=3.0))
    got = await store.get("AAPL", "equity-momentum:7", _ts(2500))
    assert got is not None and got.values["px"].scalar == 2.0  # never the later 3000


async def test_get_as_of_exact_match_inclusive(store):
    await store.put(_fv(as_of=1000, scalar=1.0))
    await store.put(_fv(as_of=2000, scalar=2.0))
    got = await store.get("AAPL", "equity-momentum:7", _ts(2000))
    assert got is not None and got.values["px"].scalar == 2.0


async def test_get_as_of_before_all_returns_none(store):
    await store.put(_fv(as_of=2000, scalar=2.0))
    assert await store.get("AAPL", "equity-momentum:7", _ts(1000)) is None


async def test_get_as_of_nanos_ordering(store):
    await store.put(_fv(as_of=1000, scalar=1.0))  # nanos=0
    fv_late = _fv(scalar=9.0)
    fv_late.as_of.CopyFrom(_ts(1000, nanos=500))
    await store.put(fv_late)
    got = await store.get("AAPL", "equity-momentum:7", _ts(1000, nanos=400))
    assert got is not None and got.values["px"].scalar == 1.0  # (sec, nanos) order


async def test_out_of_order_put_still_point_in_time_correct(store):
    await store.put(_fv(as_of=3000, scalar=3.0))
    await store.put(_fv(as_of=1000, scalar=1.0))
    await store.put(_fv(as_of=2000, scalar=2.0))
    mid = await store.get("AAPL", "equity-momentum:7", _ts(2000))
    latest = await store.get("AAPL", "equity-momentum:7")
    assert mid is not None and mid.values["px"].scalar == 2.0
    assert latest is not None and latest.values["px"].scalar == 3.0


async def test_put_same_as_of_overwrites(store):
    await store.put(_fv(as_of=1000, scalar=1.0))
    await store.put(_fv(as_of=1000, scalar=9.9))
    got = await store.get("AAPL", "equity-momentum:7", _ts(1000))
    assert got is not None and got.values["px"].scalar == 9.9


# --- Isolation + immutability -----------------------------------------


async def test_subject_and_feature_set_isolation(store):
    await store.put(_fv(subject_id="AAPL", feature_set_ref="equity-momentum:7", scalar=1.0))
    await store.put(_fv(subject_id="MSFT", feature_set_ref="equity-momentum:7", scalar=2.0))
    await store.put(_fv(subject_id="AAPL", feature_set_ref="credit-risk:3", scalar=3.0))
    assert (await store.get("AAPL", "equity-momentum:7")).values["px"].scalar == 1.0
    assert (await store.get("MSFT", "equity-momentum:7")).values["px"].scalar == 2.0
    assert (await store.get("AAPL", "credit-risk:3")).values["px"].scalar == 3.0


async def test_put_defensive_against_caller_mutation(store):
    fv = _fv(scalar=1.0)
    await store.put(fv)
    fv.values["px"].scalar = 99.0  # mutate after put — store serialized a snapshot
    got = await store.get("AAPL", "equity-momentum:7")
    assert got is not None and got.values["px"].scalar == 1.0


async def test_get_returns_independent_message(store):
    await store.put(_fv(scalar=1.0))
    got = await store.get("AAPL", "equity-momentum:7")
    got.values["px"].scalar = 42.0  # mutate the returned message
    again = await store.get("AAPL", "equity-momentum:7")
    assert again is not None and again.values["px"].scalar == 1.0


# --- Durability (the real-store difference) ---------------------------


async def test_persists_across_reopen(tmp_path):
    path = str(tmp_path / "features.db")
    s1 = SqliteFeatureStore(path)
    await s1.put(_fv(as_of=1000, scalar=1.0))
    await s1.put(_fv(as_of=2000, scalar=2.0))
    s1.close()

    s2 = SqliteFeatureStore(path)  # reopen — a fresh process would see this
    try:
        latest = await s2.get("AAPL", "equity-momentum:7")
        pit = await s2.get("AAPL", "equity-momentum:7", _ts(1500))
        assert latest is not None and latest.values["px"].scalar == 2.0
        assert pit is not None and pit.values["px"].scalar == 1.0  # point-in-time survives
    finally:
        s2.close()


# --- Validation -------------------------------------------------------


async def test_put_rejects_missing_subject_id(store):
    fv = _fv()
    fv.subject_id = ""
    with pytest.raises(ValueError, match="subject_id"):
        await store.put(fv)


async def test_put_rejects_missing_feature_set_ref(store):
    fv = _fv()
    fv.feature_set_ref = ""
    with pytest.raises(ValueError, match="feature_set_ref"):
        await store.put(fv)


async def test_put_rejects_missing_as_of(store):
    fv = FeatureVector(subject_id="AAPL", feature_set_ref="equity-momentum:7")
    with pytest.raises(ValueError, match="as_of"):
        await store.put(fv)
