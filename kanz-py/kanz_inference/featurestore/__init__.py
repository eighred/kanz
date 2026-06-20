"""Feature-store boundary (PRED-11).

The interface inference paths read materialised features through. The
boundary landed first (same lands-before-impl pattern as PRED-04's
``Model``) so a real store plugs in behind it without changing callers;
MLOPS-01f delivers that real store (``SqliteFeatureStore``).

Importable names:

- ``FeatureStore`` — Protocol: async ``get(subject_id, feature_set_ref,
  as_of)`` point-in-time read + ``put(fv)`` materialise.
- ``InMemoryFeatureStore`` — process-local trivial impl with
  point-in-time history per ``(subject_id, feature_set_ref)``.
- ``SqliteFeatureStore`` — MLOPS-01f durable, point-in-time impl on
  stdlib SQLite (file- or memory-backed); the real store the boundary
  was designed to swap in. A Feast/warehouse impl plugs in identically.
"""

from kanz_inference.featurestore.sqlite_store import SqliteFeatureStore
from kanz_inference.featurestore.store import (
    FeatureStore,
    InMemoryFeatureStore,
)

__all__ = ["FeatureStore", "InMemoryFeatureStore", "SqliteFeatureStore"]
