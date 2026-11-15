"""The first CONCRETE model on this platform (AI-M1).

Everything around it already existed — the servicer, the streaming worker, the
feature store, the explainer, the registry and its promotion gate — and every
``Model`` was a ``typing.Protocol`` with test stubs behind it. Nothing served a
prediction, so nothing consumed one.

# Why a linear model, when the brief asks for transformers and GNNs

Because the thing that was missing is not model sophistication — it is a model
AT ALL. A linear scorer is deliberately the smallest honest thing that makes the
PATH real: it produces a genuine value, a genuine confidence, and an explanation
that is exactly right (for a linear model the Shapley contribution IS w·x, which
is why the explainer's own tests are written against one).

A temporal fusion transformer, a GNN or an LLM drops in behind the identical
``Model`` Protocol — ``model_id`` plus ``async predict``. Nothing above this file
changes when one arrives. Building the transformer FIRST would have meant
building it on a gRPC service nobody served and nobody called.

# What it refuses to do

It does not score a feature vector that is missing features it was trained on. A
missing feature is not a zero — it is a HOLE in the input, and treating it as
zero produces a confident prediction from half a vector. It degrades instead, and
says which feature it could not see.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from datetime import datetime

from google.protobuf.timestamp_pb2 import Timestamp

from inference.v1.feature_vector_pb2 import FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.registry.registry import ModelMetadata, ValidationRecord

__all__ = ["LinearModel", "ModelSpec"]


@dataclass(frozen=True)
class ModelSpec:
    """A model card, in the form the service loads it from disk.

    Weights and validation evidence travel TOGETHER, deliberately: the registry
    will not promote a model to PRIMARY without a recorded, current validation
    (MLOPS-01a / SR 11-7), so a spec that cannot prove it was validated cannot
    serve. There is no code path that registers weights without the evidence.
    """

    model_id: str
    """Canonical id, ``"{name}@{version}"`` — the same string that lands in
    PredictionEnvelope.model, so a prediction can always be traced to the exact
    model that made it."""

    feature_set_ref: str
    """The feature catalog version this model consumes (``"{name}:{version}"``).
    The ROUTING KEY: the registry finds a model by the inbound feature event's
    feature_set_ref, so a model can never be fed a vector it was not trained on."""

    confidence_threshold: float
    """PRED-02 §2.3: predictions below this are DEGRADED. It is the MODEL's
    contract, not a global constant."""

    weights: dict[str, float]
    bias: float = 0.0

    validated_at: datetime | None = None
    expires_at: datetime | None = None
    validation_passed: bool = True
    artifact_uri: str = ""
    report_uri: str = ""

    scale: float = 1.0
    """Logistic scale for the confidence mapping. Larger ⇒ a given |value|
    reads as more confident."""

    def metadata(self) -> ModelMetadata:
        return ModelMetadata(
            model_id=self.model_id,
            feature_set_ref=self.feature_set_ref,
            confidence_threshold=self.confidence_threshold,
            artifact_uri=self.artifact_uri,
        )

    def validation(self) -> ValidationRecord | None:
        """The evidence the registry's primary gate requires. None when the spec
        carries no validation — which is exactly what makes it unservable."""
        if self.validated_at is None or self.expires_at is None:
            return None
        return ValidationRecord(
            model_id=self.model_id,
            validated_at=self.validated_at,
            expires_at=self.expires_at,
            passed=self.validation_passed,
            report_uri=self.report_uri,
        )


class LinearModel:
    """value = bias + Σ wᵢ·xᵢ over the scalar features named in ``weights``."""

    def __init__(self, spec: ModelSpec) -> None:
        if not spec.weights:
            raise ValueError("linear model: no weights — it would score every input the same")
        self._spec = spec
        self.model_id = spec.model_id

    @property
    def spec(self) -> ModelSpec:
        return self._spec

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        as_of = Timestamp()
        as_of.CopyFrom(fv.as_of)

        missing = sorted(name for name in self._spec.weights if name not in fv.values)
        if missing:
            # A HOLE IN THE INPUT, not a zero. Scoring it would produce a
            # confident number from a partial vector — the most dangerous kind
            # of wrong, because it is indistinguishable from a real one.
            return PredictionEnvelope(
                subject_id=fv.subject_id,
                model=self.model_id,
                value=0.0,
                confidence=0.0,  # unknown confidence IS no confidence (PRED-02)
                mode=PredictionMode.PREDICTION_MODE_DEGRADED,
                degraded_reason="missing features: " + ", ".join(missing),
                as_of=as_of,
            )

        value = self._spec.bias
        for name, w in self._spec.weights.items():
            value += w * fv.values[name].scalar

        confidence = self._confidence(value)
        degraded = confidence < self._spec.confidence_threshold

        return PredictionEnvelope(
            subject_id=fv.subject_id,
            model=self.model_id,
            value=value,
            confidence=confidence,
            mode=(
                PredictionMode.PREDICTION_MODE_DEGRADED
                if degraded
                else PredictionMode.PREDICTION_MODE_NORMAL
            ),
            degraded_reason=(
                f"confidence {confidence:.3f} below the model's threshold "
                f"{self._spec.confidence_threshold:.3f}"
                if degraded
                else ""
            ),
            as_of=as_of,
        )

    def _confidence(self, value: float) -> float:
        """A logistic map of |value| onto (0, 1].

        This is a CALIBRATION CLAIM and it is a weak one: the confidence a linear
        scorer can honestly report is a monotone function of how far its output
        sits from the decision boundary, and nothing more. It is stated here
        rather than hidden so that the day a model with a real calibrated
        probability arrives, it is obvious what it replaces.

        A model with NO calibrated confidence must set confidence_threshold=1.0,
        which marks every prediction DEGRADED — PRED-02's "unknown confidence as
        no confidence" discipline, and the correct posture for a model that
        cannot say how much it trusts itself.
        """
        z = abs(value) * self._spec.scale
        return 2.0 / (1.0 + math.exp(-z)) - 1.0
