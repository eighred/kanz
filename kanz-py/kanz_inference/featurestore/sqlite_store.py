"""Durable point-in-time feature store (MLOPS-01f).

The PRED-11 boundary was landed "interface first, real store later"; this
is the real store, plugged in behind the same ``FeatureStore`` protocol
with zero caller changes — the swap the boundary was designed for.

# Why SQLite, not Feast

The board says "Feast or equivalent". Feast pulls a heavy dependency tree
and assumes external infra (an online store + an offline warehouse),
which the kanz-py test suite deliberately runs without (the only skipped
tests need live NATS/Kafka). The project's standing rules — avoid
dependency bloat, keep the suite self-contained — point to the same
choice the Go side made for the market-data plane (an in-memory
``store.Memory`` alongside a durable ``store.Postgres``): a durable
implementation on a dependency-free engine. SQLite (stdlib ``sqlite3``)
gives real persistence and a real point-in-time SQL query, file- or
memory-backed, with nothing to install. A Feast / warehouse / Redis-online
implementation plugs in behind the identical ``get``/``put`` contract when
the deployment needs it; the point-in-time semantics proven here are the
contract every such store must honour.

# Bitemporal-free, but point-in-time correct

Like the in-memory impl, history is keyed by ``(subject_id,
feature_set_ref, as_of)`` and ``get`` returns the row effective *at or
before* the requested ``as_of`` — never a later one. The durable
difference is that the history survives process restart, so a backtest
run tomorrow against today's materialisations reads exactly what was
known then.

# Async over a sync driver

``sqlite3`` is synchronous; the protocol is async so remote/IO stores
fit. Each call dispatches its blocking work to a worker thread via
``asyncio.to_thread`` so the event loop is never blocked, and a single
shared connection (``check_same_thread=False``) is guarded by a lock
because the thread pool may run calls on different threads. A
production Postgres/warehouse impl would use an async driver directly;
the contract is identical either way.
"""

from __future__ import annotations

import asyncio
import sqlite3
import threading
from typing import Optional

from google.protobuf.timestamp_pb2 import Timestamp
from inference.v1.feature_vector_pb2 import FeatureVector

from kanz_inference.featurestore.store import _require_put_fields

_SCHEMA = """
CREATE TABLE IF NOT EXISTS feature_vectors (
    subject_id      TEXT    NOT NULL,
    feature_set_ref TEXT    NOT NULL,
    as_of_seconds   INTEGER NOT NULL,
    as_of_nanos     INTEGER NOT NULL,
    payload         BLOB    NOT NULL,
    PRIMARY KEY (subject_id, feature_set_ref, as_of_seconds, as_of_nanos)
);
"""


class SqliteFeatureStore:
    """Durable ``FeatureStore`` backed by SQLite. ``path=":memory:"``
    (the default) is process-local and lost on close; a file path
    persists across restarts."""

    def __init__(self, path: str = ":memory:") -> None:
        # check_same_thread=False: asyncio.to_thread may run our blocking
        # calls on different pool threads. The lock serialises access so
        # the single connection is never touched concurrently.
        self._conn = sqlite3.connect(path, check_same_thread=False)
        self._lock = threading.Lock()
        with self._lock:
            self._conn.executescript(_SCHEMA)
            self._conn.commit()

    async def get(
        self,
        subject_id: str,
        feature_set_ref: str,
        as_of: Optional[Timestamp] = None,
    ) -> Optional[FeatureVector]:
        bound = (as_of.seconds, as_of.nanos) if as_of is not None else None
        payload = await asyncio.to_thread(self._get_sync, subject_id, feature_set_ref, bound)
        if payload is None:
            return None
        fv = FeatureVector()
        fv.ParseFromString(payload)
        return fv

    async def put(self, fv: FeatureVector) -> None:
        _require_put_fields(fv)
        await asyncio.to_thread(
            self._put_sync,
            fv.subject_id,
            fv.feature_set_ref,
            fv.as_of.seconds,
            fv.as_of.nanos,
            fv.SerializeToString(),
        )

    def close(self) -> None:
        with self._lock:
            self._conn.close()

    # --- sync DB work (runs on a worker thread) ------------------------

    def _get_sync(
        self,
        subject_id: str,
        feature_set_ref: str,
        bound: Optional[tuple[int, int]],
    ) -> Optional[bytes]:
        with self._lock:
            if bound is None:
                row = self._conn.execute(
                    "SELECT payload FROM feature_vectors "
                    "WHERE subject_id=? AND feature_set_ref=? "
                    "ORDER BY as_of_seconds DESC, as_of_nanos DESC LIMIT 1",
                    (subject_id, feature_set_ref),
                ).fetchone()
            else:
                seconds, nanos = bound
                # Rightmost row with (as_of_seconds, as_of_nanos) <= bound.
                row = self._conn.execute(
                    "SELECT payload FROM feature_vectors "
                    "WHERE subject_id=? AND feature_set_ref=? "
                    "AND (as_of_seconds < ? OR (as_of_seconds = ? AND as_of_nanos <= ?)) "
                    "ORDER BY as_of_seconds DESC, as_of_nanos DESC LIMIT 1",
                    (subject_id, feature_set_ref, seconds, seconds, nanos),
                ).fetchone()
        return row[0] if row is not None else None

    def _put_sync(
        self,
        subject_id: str,
        feature_set_ref: str,
        seconds: int,
        nanos: int,
        payload: bytes,
    ) -> None:
        with self._lock:
            # INSERT OR REPLACE: re-materialising the same as_of overwrites,
            # a new as_of extends the subject's point-in-time history.
            self._conn.execute(
                "INSERT OR REPLACE INTO feature_vectors "
                "(subject_id, feature_set_ref, as_of_seconds, as_of_nanos, payload) "
                "VALUES (?, ?, ?, ?, ?)",
                (subject_id, feature_set_ref, seconds, nanos, payload),
            )
            self._conn.commit()
