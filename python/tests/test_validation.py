"""MLOPS-01b validation-suite tests."""

from __future__ import annotations

import math
from datetime import datetime, timedelta, timezone

import pytest

from inference.v1.feature_vector_pb2 import FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.registry import ModelMetadata, Registry, ValidationError
from kanz_inference.validation import (
    ValidationSample,
    ValidationThresholds,
    Validator,
)


class _ScriptedModel:
    """Returns (value, confidence) pairs in call order — decouples the
    model's output from the sample's actual so any (pred, conf, actual)
    combination can be exercised."""

    def __init__(self, model_id: str, values, confidences):
        self.model_id = model_id
        self._values = list(values)
        self._confs = list(confidences)
        self._i = 0

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        v, c = self._values[self._i], self._confs[self._i]
        self._i += 1
        return PredictionEnvelope(
            subject_id=fv.subject_id,
            model=self.model_id,
            value=v,
            confidence=c,
            mode=PredictionMode.PREDICTION_MODE_NORMAL,
        )


def _samples(actuals) -> list[ValidationSample]:
    out = []
    for i, a in enumerate(actuals):
        fv = FeatureVector()
        fv.subject_id = f"S{i}"
        fv.feature_set_ref = "equity-momentum:7"
        out.append(ValidationSample(feature_vector=fv, actual=float(a)))
    return out


# --- Holdout metrics --------------------------------------------------


async def test_holdout_metrics_are_hand_computable():
    # preds [1,2,3] vs actuals [1,1,1] ⇒ errors [0,1,2]:
    # rmse=sqrt(5/3), mae=1, bias=1.
    v = Validator(ValidationThresholds(min_samples=1))
    model = _ScriptedModel("m@1", [1.0, 2.0, 3.0], [0.9, 0.9, 0.9])
    res = await v.validate(model, _samples([1.0, 1.0, 1.0]))
    assert res.metrics.n == 3
    assert res.metrics.rmse == pytest.approx(math.sqrt(5 / 3))
    assert res.metrics.mae == pytest.approx(1.0)
    assert res.metrics.bias == pytest.approx(1.0)


async def test_too_few_samples_auto_fails():
    v = Validator(ValidationThresholds(min_samples=30))
    model = _ScriptedModel("m@1", [1.0], [1.0])
    res = await v.validate(model, _samples([1.0]))
    assert res.passed is False
    assert res.recommended_confidence_threshold == 1.0
    assert any("samples" in r for r in res.reasons)


async def test_rmse_bound_fails_when_exceeded():
    v = Validator(ValidationThresholds(min_samples=1, max_rmse=0.5))
    model = _ScriptedModel("m@1", [5.0, 5.0, 5.0], [0.9, 0.9, 0.9])
    res = await v.validate(model, _samples([0.0, 0.0, 0.0]))  # rmse=5
    assert res.passed is False
    assert any("rmse" in r for r in res.reasons)


async def test_bias_bound_detects_systematic_overprediction():
    v = Validator(ValidationThresholds(min_samples=1, max_abs_bias=0.5))
    model = _ScriptedModel("m@1", [2.0, 2.0, 2.0], [0.9, 0.9, 0.9])
    res = await v.validate(model, _samples([0.0, 0.0, 0.0]))  # bias=+2
    assert res.metrics.bias == pytest.approx(2.0)
    assert res.passed is False
    assert any("bias" in r for r in res.reasons)


# --- Calibration ------------------------------------------------------


async def test_all_zero_confidence_is_uncalibrated():
    # The proto's "no calibrated confidence" sentinel ⇒ always DEGRADED.
    v = Validator(ValidationThresholds(min_samples=1))
    n = 10
    model = _ScriptedModel("m@1", [1.0] * n, [0.0] * n)  # correct but no confidence
    res = await v.validate(model, _samples([1.0] * n))
    assert res.metrics.calibrated is False
    assert res.recommended_confidence_threshold == 1.0
    assert any("uncalibrated" in r for r in res.reasons)


async def test_overconfident_wrong_model_is_uncalibrated():
    # confidence 1.0 but every prediction wrong ⇒ ECE≈1 ⇒ uncalibrated.
    v = Validator(ValidationThresholds(min_samples=1))
    n = 10
    model = _ScriptedModel("m@1", [9.0] * n, [1.0] * n)
    res = await v.validate(model, _samples([0.0] * n))
    assert res.metrics.calibrated is False
    assert res.metrics.calibration_error == pytest.approx(1.0, abs=1e-9)
    assert res.recommended_confidence_threshold == 1.0


async def test_well_calibrated_model_recommends_a_usable_threshold():
    # High confidence ⇒ correct; the model is calibrated and the
    # recommended threshold is a real confidence (< 1.0), not the
    # always-DEGRADED sentinel.
    v = Validator(ValidationThresholds(min_samples=1, target_accuracy=0.7))
    n = 20
    model = _ScriptedModel("m@1", [1.0] * n, [0.95] * n)  # all correct, confident
    res = await v.validate(model, _samples([1.0] * n))
    assert res.metrics.calibrated is True
    assert res.metrics.calibration_error <= 0.1
    assert res.recommended_confidence_threshold == pytest.approx(0.95)


# --- Stability --------------------------------------------------------


async def test_stable_error_across_window_passes_stability():
    v = Validator(ValidationThresholds(min_samples=1))
    # Uniform error of +0.5 throughout ⇒ ratio ≈ 1.
    model = _ScriptedModel("m@1", [0.5] * 8, [0.9] * 8)
    res = await v.validate(model, _samples([0.0] * 8))
    assert res.metrics.stability_ratio == pytest.approx(1.0)


async def test_degrading_error_fails_stability():
    v = Validator(ValidationThresholds(min_samples=1, max_stability_ratio=2.0))
    # First half perfect, second half off by 1 ⇒ ratio explodes.
    model = _ScriptedModel("m@1", [0.0, 0.0, 1.0, 1.0], [0.9, 0.9, 0.9, 0.9])
    res = await v.validate(model, _samples([0.0, 0.0, 0.0, 0.0]))
    assert res.metrics.stability_ratio > 2.0
    assert res.passed is False
    assert any("stability" in r for r in res.reasons)


# --- Result → record → registry gate (ties MLOPS-01b to 01a) ----------


async def test_to_record_stamps_validity_window():
    v = Validator(ValidationThresholds(min_samples=1))
    model = _ScriptedModel("m@1", [1.0] * 5, [0.9] * 5)
    res = await v.validate(model, _samples([1.0] * 5))
    now = datetime(2026, 6, 21, tzinfo=timezone.utc)
    rec = res.to_record(now=now, validity=timedelta(days=30))
    assert rec.model_id == "m@1"
    assert rec.validated_at == now
    assert rec.expires_at == now + timedelta(days=30)
    assert rec.passed is res.passed


async def test_passing_validation_unlocks_primary_registration():
    r = Registry()
    meta = ModelMetadata(
        model_id="vol-forecast@1.4.2",
        feature_set_ref="equity-momentum:7",
        confidence_threshold=0.7,
        artifact_uri="s3://x/m",
    )
    model = _ScriptedModel(meta.model_id, [1.0] * 40, [0.95] * 40)
    res = await Validator(ValidationThresholds(min_samples=1)).validate(
        model, _samples([1.0] * 40)
    )
    assert res.passed is True

    r.record_validation(res.to_record())
    r.register(meta, model)  # gate satisfied — no raise
    assert r.primary_for_feature_set(meta.feature_set_ref)[0].model_id == meta.model_id


async def test_failing_validation_blocks_primary_registration():
    r = Registry()
    meta = ModelMetadata(
        model_id="bad@0.1.0",
        feature_set_ref="equity-momentum:7",
        confidence_threshold=0.7,
        artifact_uri="s3://x/bad",
    )
    # Fails the rmse bound ⇒ passed False ⇒ recorded but gate rejects.
    model = _ScriptedModel(meta.model_id, [9.0] * 40, [0.9] * 40)
    res = await Validator(
        ValidationThresholds(min_samples=1, max_rmse=0.5)
    ).validate(model, _samples([0.0] * 40))
    assert res.passed is False

    r.record_validation(res.to_record())
    with pytest.raises(ValidationError):
        r.register(meta, model)
