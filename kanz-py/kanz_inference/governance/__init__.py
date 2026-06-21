"""Model governance (MLOps) — the automated loops that keep models
trustworthy in production.

MLOPS-01c: the drift → revalidation loop. A DATA-07
``data.feature.drift_detected`` event for a feature a model depends on
triggers an automated revalidation/retrain signal (the model's recorded
MLOPS-01a validation can no longer be trusted once its inputs drift).

MLOPS-01e: champion/challenger promotion. ``ShadowComparisonStore`` (a
PRED-10 ShadowObserver) accumulates comparison metrics; ``Promoter``
promotes a challenger gated on soak + validation + out-performance.

MLOPS-01g: model cards + approval workflow + lineage. ``ModelCard`` +
``LineageRecord`` document a model and its model→feature→training-data
provenance; ``ApprovalWorkflow`` is the DRAFT→PENDING→APPROVED sign-off
gate (approval requires a passing validation).

Importable names:

- ``DriftTrigger`` — consumes drift events, fans out a RevalidationRequest
  per affected model (with a per-(model, feature) cooldown).
- ``RevalidationRequest`` — the emitted signal, carrying the drift evidence.
- ``ModelResolver`` / ``StaticModelResolver`` — feature → affected model_ids.
- ``RevalidationSink`` / ``LoggingRevalidationSink`` — where the signal goes.
- ``SUBJECT_DRIFT_DETECTED`` — the subscription subject.
- ``ShadowComparisonStore`` — ShadowObserver accumulating champion/challenger
  comparison metrics (+ ``record_outcome`` for live RMSE).
- ``ComparisonStats`` — read view of one pair's accumulated metrics.
- ``Promoter`` / ``PromotionDecision`` — the gated promotion evaluator.
- ``ModelCard`` / ``LineageRecord`` — model documentation + provenance.
- ``ApprovalWorkflow`` / ``ApprovalStatus`` / ``ApprovalState`` /
  ``ApprovalError`` — the sign-off state machine.
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
from kanz_inference.governance.model_card import (
    ApprovalError,
    ApprovalState,
    ApprovalStatus,
    ApprovalWorkflow,
    LineageRecord,
    ModelCard,
)
from kanz_inference.governance.promotion import (
    ComparisonStats,
    Promoter,
    PromotionDecision,
    ShadowComparisonStore,
)

__all__ = [
    "SUBJECT_DRIFT_DETECTED",
    "DriftTrigger",
    "LoggingRevalidationSink",
    "ModelResolver",
    "RevalidationRequest",
    "RevalidationSink",
    "StaticModelResolver",
    "ComparisonStats",
    "Promoter",
    "PromotionDecision",
    "ShadowComparisonStore",
    "ApprovalError",
    "ApprovalState",
    "ApprovalStatus",
    "ApprovalWorkflow",
    "LineageRecord",
    "ModelCard",
]
