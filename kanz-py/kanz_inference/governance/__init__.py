"""Model governance (MLOps) — the automated loops that keep models
trustworthy in production.

MLOPS-01c: the drift → revalidation loop. A DATA-07
``data.feature.drift_detected`` event for a feature a model depends on
triggers an automated revalidation/retrain signal (the model's recorded
MLOPS-01a validation can no longer be trusted once its inputs drift).

Importable names:

- ``DriftTrigger`` — consumes drift events, fans out a RevalidationRequest
  per affected model (with a per-(model, feature) cooldown).
- ``RevalidationRequest`` — the emitted signal, carrying the drift evidence.
- ``ModelResolver`` / ``StaticModelResolver`` — feature → affected model_ids.
- ``RevalidationSink`` / ``LoggingRevalidationSink`` — where the signal goes.
- ``SUBJECT_DRIFT_DETECTED`` — the subscription subject.
"""

from kanz_inference.governance.drift_trigger import (
    SUBJECT_DRIFT_DETECTED,
    DriftTrigger,
    LoggingRevalidationSink,
    ModelResolver,
    RevalidationRequest,
    RevalidationSink,
    StaticModelResolver,
)

__all__ = [
    "SUBJECT_DRIFT_DETECTED",
    "DriftTrigger",
    "LoggingRevalidationSink",
    "ModelResolver",
    "RevalidationRequest",
    "RevalidationSink",
    "StaticModelResolver",
]
