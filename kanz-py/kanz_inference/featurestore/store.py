"""Feature-store boundary — the interface inference paths call to
read materialised features for a subject at a point in time, plus a
trivial in-memory implementation.

# Why a boundary now, a real store later

Today both inference paths receive a full ``FeatureVector`` already:
the streaming worker (PRED-04) decodes it from the
``inference.feature.computed`` event, the interactive servicer
(PRED-08) gets it as the gRPC request body. So nothing yet *needs* a
feature store. PRED-11 lands the boundary anyway — the same
lands-before-impl pattern as RISK-04's ``Applier`` and PRED-04's
``Model``/``PredictionPublisher`` — so the contract is fixed before a
real store (Feast, Redis, DynamoDB, a point-in-time warehouse) plugs
in, and PRED-12's schema-versioning contract tests have something to
target. Callers depend on ``FeatureStore``; the trivial impl and any
production impl are interchangeable behind it.

# Point-in-time reads are the load-bearing decision

``get`` takes an ``as_of`` and returns the feature vector effective
*at or before* that time — never a later one. This is the single
property that distinguishes a feature store from a cache:

- Replay / backtest determinism: replaying yesterday's events must
  see the features as they were yesterday, not whatever is latest
  now. A "latest value wins" store would silently leak future data
  into a replay, breaking the EVT-21d-style determinism the rest of
  the platform guarantees.
- Training/serving skew: offline training joins features as-of the
  label time; online serving must answer with the same as-of
  semantics or the model sees a different distribution than it was
  trained on.

A store that ignored ``as_of`` would be cheaper and look correct in a
live-only demo, then corrupt every backtest. The boundary forces the
honest contract on every implementation.

# What the trivial impl deliberately does NOT do

- Persistence — process-local dict, lost on restart. A real store is
  the durable system of record.
- TTL / eviction — unbounded growth. Fine for tests + small
  deployments; a real store ages out cold subjects.
- Online/offline split — production feature stores keep a low-latency
  online store and a high-throughput offline store in sync; that's a
  deployment concern behind the same ``get``/``put`` contract.
- Distributed state — single process, like the PRED-09 registry. No
  cross-process coordination.
"""

from __future__ import annotations

import bisect
from typing import Optional, Protocol

from google.protobuf.timestamp_pb2 import Timestamp
from inference.v1.feature_vector_pb2 import FeatureVector


def _as_of_key(ts: Timestamp) -> tuple[int, int]:
    """Comparable, exact ordering key for a Timestamp. Proto
    Timestamps aren't directly orderable; ``(seconds, nanos)`` is the
    exact lexicographic order without float round-trip.
    """
    return (ts.seconds, ts.nanos)


def _clone(fv: FeatureVector) -> FeatureVector:
    """Defensive copy. The store owns its entries; returning or
    storing the caller's live message would let later mutation bleed
    into stored state (or stored state into the caller) — the same
    immutability discipline RISK-03's domain accessors keep.
    """
    out = FeatureVector()
    out.CopyFrom(fv)
    return out


class FeatureStore(Protocol):
    """The boundary inference paths read features through. Async so a
    remote store (Feast online store, Redis, DynamoDB, a warehouse
    point-in-time query) plugs in without changing the contract — the
    same async-everywhere choice the ``Model`` protocol makes.

    Keyed by ``(subject_id, feature_set_ref)``: a subject can carry
    features under several feature sets at once (different models read
    different sets), so the feature_set_ref is part of the identity,
    not just routing.
    """

    async def get(
        self,
        subject_id: str,
        feature_set_ref: str,
        as_of: Optional[Timestamp] = None,
    ) -> Optional[FeatureVector]:
        """The feature vector for ``subject_id`` under
        ``feature_set_ref`` effective at or before ``as_of``. When
        ``as_of`` is ``None``, the most recent vector. ``None`` if no
        vector exists at or before that time (the caller treats a miss
        as missing-features → DEGRADED per PRED-02, NOT as zero
        features)."""
        ...

    async def put(self, fv: FeatureVector) -> None:
        """Materialise a feature vector. Indexed by its
        ``(subject_id, feature_set_ref, as_of)``; storing the same
        as_of again overwrites, a new as_of extends the subject's
        history so later point-in-time reads see it."""
        ...


class InMemoryFeatureStore:
    """Trivial process-local ``FeatureStore``. One time-ordered
    history per ``(subject_id, feature_set_ref)``; ``get`` does a
    point-in-time lookup, ``put`` inserts keeping the history sorted.

    Single asyncio event loop ⇒ no lock; the methods are async to
    satisfy the protocol but never await, so each runs to completion
    atomically under cooperative scheduling — same reasoning as
    PRED-04's lock-free ``LastKnownCache``.
    """

    def __init__(self) -> None:
        # (subject_id, feature_set_ref) → history sorted ascending by
        # as_of key. Parallel _keys list holds the as_of keys so
        # bisect can binary-search without recomputing them.
        self._history: dict[tuple[str, str], list[FeatureVector]] = {}
        self._keys: dict[tuple[str, str], list[tuple[int, int]]] = {}

    async def get(
        self,
        subject_id: str,
        feature_set_ref: str,
        as_of: Optional[Timestamp] = None,
    ) -> Optional[FeatureVector]:
        history = self._history.get((subject_id, feature_set_ref))
        if not history:
            return None

        if as_of is None:
            return _clone(history[-1])

        keys = self._keys[(subject_id, feature_set_ref)]
        # Rightmost entry with as_of <= target. bisect_right on the
        # target key gives the count of entries at-or-before it; the
        # one before that index is the answer.
        idx = bisect.bisect_right(keys, _as_of_key(as_of))
        if idx == 0:
            # Every stored vector is strictly newer than the requested
            # time — no point-in-time answer (would be future leakage).
            return None
        return _clone(history[idx - 1])

    async def put(self, fv: FeatureVector) -> None:
        if not fv.subject_id:
            raise ValueError("FeatureVector.subject_id required")
        if not fv.feature_set_ref:
            raise ValueError("FeatureVector.feature_set_ref required")
        if not fv.HasField("as_of"):
            raise ValueError("FeatureVector.as_of required for point-in-time storage")

        key = (fv.subject_id, fv.feature_set_ref)
        history = self._history.setdefault(key, [])
        keys = self._keys.setdefault(key, [])

        as_of_key = _as_of_key(fv.as_of)
        idx = bisect.bisect_left(keys, as_of_key)
        stored = _clone(fv)
        if idx < len(keys) and keys[idx] == as_of_key:
            # Same as_of already present → overwrite (idempotent
            # re-materialisation of one point in time).
            history[idx] = stored
        else:
            keys.insert(idx, as_of_key)
            history.insert(idx, stored)
