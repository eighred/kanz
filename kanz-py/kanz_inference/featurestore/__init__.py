"""Feature-store boundary (PRED-11).

The interface inference paths read materialised features through, plus
a trivial in-memory implementation. Both inference paths currently
receive a full ``FeatureVector`` already, so nothing consumes the
store yet — the boundary lands first (same lands-before-impl pattern
as PRED-04's ``Model``) so a real store (Feast, Redis, a point-in-time
warehouse) plugs in behind it without changing callers, and PRED-12's
schema-versioning contract tests have a target.

Importable names:

- ``FeatureStore`` — Protocol: async ``get(subject_id, feature_set_ref,
  as_of)`` point-in-time read + ``put(fv)`` materialise.
- ``InMemoryFeatureStore`` — process-local trivial impl with
  point-in-time history per ``(subject_id, feature_set_ref)``.
"""

from kanz_inference.featurestore.store import (
    FeatureStore,
    InMemoryFeatureStore,
)

__all__ = ["FeatureStore", "InMemoryFeatureStore"]
