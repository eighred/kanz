"""MLOPS-01h — epic-closing integration tests for the model-governance
loop. Unlike the per-module unit tests, these wire the real components
together (registry + validator + shadow executor + comparison store +
promoter + drift trigger + explaining model) and assert the four
behaviors the board names end-to-end.
"""

from __future__ import annotations

from datetime import datetime, timezone

import pytest

from envelope.v1.envelope_pb2 import Envelope
from inference.v1.feature_vector_pb2 import FeatureValue, FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode
from observation.v1.data_quality_pb2 import DataQualityEvent, Severity

from kanz_inference.registry import ModelMetadata, Registry
from kanz_inference.explain import ExplainingModel, PermutationExplainer
from kanz_inference.governance import (
    DriftTrigger,
    Promoter,
    RevalidationRequest,
    ShadowComparisonStore,
    StaticModelResolver,
)
from kanz_inference.shadow import ShadowExecutor
from kanz_inference.validation import ValidationSample, ValidationThresholds, Validator

FS = "equity-momentum:7"


class FixedModel:
    """Returns a constant value (+confidence) for every input — lets a
    test pin each model's error against a chosen outcome."""

    def __init__(self, model_id: str, value: float, confidence: float = 0.9):
        self.model_id = model_id
        self._v = value
        self._c = confidence

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        return PredictionEnvelope(
            subject_id=fv.subject_id, model=self.model_id, value=self._v,
            confidence=self._c, mode=PredictionMode.PREDICTION_MODE_NORMAL,
        )


class LinearModel:
    model_id = "lin@1"

    def __init__(self, weights: dict[str, float]):
        self.w = weights

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        v = sum(self.w.get(n, 0.0) * fval.scalar for n, fval in fv.values.items() if fval.HasField("scalar"))
        return PredictionEnvelope(
            subject_id=fv.subject_id, model=self.model_id, value=v,
            mode=PredictionMode.PREDICTION_MODE_NORMAL,
        )


def _meta(model_id: str) -> ModelMetadata:
    return ModelMetadata(model_id=model_id, feature_set_ref=FS, confidence_threshold=0.7, artifact_uri=f"s3://x/{model_id}")


def _fv(subject: str, **scalars: float) -> FeatureVector:
    fv = FeatureVector(subject_id=subject, feature_set_ref=FS)
    for name, x in (scalars or {"x": 1.0}).items():
        fv.values[name].CopyFrom(FeatureValue(scalar=x))
    return fv


async def _validate_and_record(registry: Registry, validator: Validator, model, actual: float, n: int = 5):
    """Run the real validation suite and file the resulting record — the
    same path MLOPS-01b → 01a takes in production."""
    samples = [ValidationSample(_fv(f"V{i}"), actual) for i in range(n)]
    result = await validator.validate(model, samples)
    assert result.passed
    registry.record_validation(result.to_record())


def _drift_payload(feature: str) -> bytes:
    event = DataQualityEvent(subject="inference.feature.computed", partition_key="AAPL", severity=Severity.SEVERITY_CRITICAL, summary="drift")
    event.drift.feature = feature
    event.drift.metric = "psi"
    event.drift.score = 0.4
    event.drift.threshold = 0.2
    return event.SerializeToString()


# --- 1. promotion blocked without validation --------------------------


async def test_promotion_blocked_without_validation_then_unblocked():
    registry = Registry()
    validator = Validator(ValidationThresholds(min_samples=1))
    champ, chal = FixedModel("champ@1", 2.0), FixedModel("chal@1", 1.0)

    await _validate_and_record(registry, validator, champ, actual=2.0)
    registry.register(_meta("champ@1"), champ)               # validated primary
    registry.register(_meta("chal@1"), chal, primary=False)  # shadow, NOT validated

    store = ShadowComparisonStore()
    executor = ShadowExecutor(registry, observer=store)
    for i in range(5):  # champ→2.0, chal→1.0, actual 1.0 ⇒ challenger far better
        await executor.predict(_fv(f"S{i}"))
        await executor.wait_for_shadows()
        store.record_outcome(f"S{i}", 1.0)

    promoter = Promoter(registry, store, min_observations=3, min_scored=3)
    blocked = promoter.evaluate(FS)
    assert blocked.promoted is None  # out-performs, but no validation
    assert any("validation" in r for r in blocked.reasons)

    # Now validate the challenger (MLOPS-01b) and retry — the gate clears.
    await _validate_and_record(registry, validator, chal, actual=1.0)
    unblocked = promoter.evaluate(FS)
    assert unblocked.promoted == "chal@1"
    assert registry.primary_for_feature_set(FS)[0].model_id == "chal@1"


# --- 2. drift triggers revalidation -----------------------------------


async def test_drift_triggers_revalidation():
    registry = Registry()
    validator = Validator(ValidationThresholds(min_samples=1))
    model = FixedModel("m@1", 1.0)
    await _validate_and_record(registry, validator, model, actual=1.0)
    registry.register(_meta("m@1"), model)
    first_validation = registry.validation_for("m@1")

    captured: list[RevalidationRequest] = []

    class _Sink:
        async def request_revalidation(self, request: RevalidationRequest) -> None:
            captured.append(request)

    trigger = DriftTrigger(StaticModelResolver({"rsi_14": ["m@1"]}), _Sink())
    await trigger.handle(Envelope(), _drift_payload("rsi_14"))

    # The drift produced a revalidation signal for the model.
    assert [r.model_id for r in captured] == ["m@1"]
    assert captured[0].feature == "rsi_14"

    # Actuate the signal: revalidate on fresh data and refile the record —
    # the model carries current validation again.
    await _validate_and_record(registry, validator, model, actual=1.0)
    refreshed = registry.validation_for("m@1")
    assert refreshed is not first_validation
    assert refreshed.is_valid(datetime.now(timezone.utc))


# --- 3. explanation present on every prediction -----------------------


async def test_explanation_present_on_every_normal_prediction():
    inner = LinearModel({"a": 1.5, "b": -0.5, "c": 2.0})
    model = ExplainingModel(inner, PermutationExplainer(samples=6, seed=3))
    for i in range(5):
        pred = await model.predict(_fv(f"S{i}", a=float(i + 1), b=2.0, c=0.5))
        assert pred.mode == PredictionMode.PREDICTION_MODE_NORMAL
        # Every consumed feature has a contribution.
        assert set(pred.explanation.keys()) == {"a", "b", "c"}


# --- 4. champion/challenger selection correctness ---------------------


async def test_champion_challenger_selects_best_challenger():
    registry = Registry()
    validator = Validator(ValidationThresholds(min_samples=1))
    champ = FixedModel("champ@1", 2.0)
    chal_a = FixedModel("chal-a@1", 1.5)
    chal_b = FixedModel("chal-b@1", 1.2)  # closest to actual 1.0 ⇒ best

    await _validate_and_record(registry, validator, champ, actual=2.0)
    registry.register(_meta("champ@1"), champ)
    for m in (chal_a, chal_b):
        await _validate_and_record(registry, validator, m, actual=1.0)
        registry.register(_meta(m.model_id), m, primary=False)

    store = ShadowComparisonStore()
    executor = ShadowExecutor(registry, observer=store)
    for i in range(5):
        await executor.predict(_fv(f"S{i}"))
        await executor.wait_for_shadows()
        store.record_outcome(f"S{i}", 1.0)  # champ err 1.0, A 0.5, B 0.2

    decision = Promoter(registry, store, min_observations=3, min_scored=3).evaluate(FS)
    assert decision.promoted == "chal-b@1"  # the lowest-RMSE challenger
    assert registry.primary_for_feature_set(FS)[0].model_id == "chal-b@1"
    # The other challenger and the old champion remain as shadows.
    assert {m.model_id for m, _ in registry.shadows_for_feature_set(FS)} == {"chal-a@1", "champ@1"}
