"""MLOPS-01d explainability tests."""

from __future__ import annotations

import pytest

from inference.v1.feature_vector_pb2 import FeatureValue, FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.explain import ExplainingModel, PermutationExplainer


# --- Models -----------------------------------------------------------


class LinearModel:
    """value = Σ w_i · x_i over present scalar features. Absent ⇒ 0.
    For a linear model the exact Shapley value of feature i is w_i·x_i."""

    model_id = "lin@1"

    def __init__(self, weights: dict[str, float]):
        self.w = weights

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        v = 0.0
        for name, fval in fv.values.items():
            if fval.HasField("scalar"):
                v += self.w.get(name, 0.0) * fval.scalar
        return PredictionEnvelope(
            subject_id=fv.subject_id,
            model=self.model_id,
            value=v,
            mode=PredictionMode.PREDICTION_MODE_NORMAL,
        )


class InteractionModel:
    """value = x_a · x_b — non-additive, to exercise efficiency on a model
    whose attributions are not simply per-feature."""

    model_id = "inter@1"

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        def s(name: str) -> float:
            fval = fv.values.get(name)
            return fval.scalar if fval is not None and fval.HasField("scalar") else 0.0

        return PredictionEnvelope(
            subject_id=fv.subject_id,
            model=self.model_id,
            value=s("a") * s("b"),
            mode=PredictionMode.PREDICTION_MODE_NORMAL,
        )


class StrictModel:
    """Raises unless every required feature is present — models the
    'refuses partial input' case the explainer must tolerate."""

    model_id = "strict@1"
    required = {"a", "b"}

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        if not self.required <= set(fv.values.keys()):
            raise ValueError("missing required features")
        return PredictionEnvelope(
            subject_id=fv.subject_id, model=self.model_id, value=1.0,
            mode=PredictionMode.PREDICTION_MODE_NORMAL,
        )


def _fv(scalars: dict[str, float]) -> FeatureVector:
    fv = FeatureVector(subject_id="AAPL", feature_set_ref="equity-momentum:7")
    for name, x in scalars.items():
        fv.values[name].CopyFrom(FeatureValue(scalar=x))
    return fv


# --- Attribution correctness ------------------------------------------


async def test_linear_model_attributes_w_times_x():
    model = LinearModel({"a": 1.5, "b": -0.5})
    fv = _fv({"a": 2.0, "b": 4.0})  # value = 3 - 2 = 1
    expl = await PermutationExplainer(samples=8, seed=1).explain(model, fv)
    assert expl["a"] == pytest.approx(3.0)   # 1.5 * 2
    assert expl["b"] == pytest.approx(-2.0)  # -0.5 * 4


async def test_efficiency_contributions_sum_to_full_minus_baseline():
    # Holds exactly for any model (telescoping per-permutation), even
    # a non-additive one — the proto's "value minus baseline" total.
    model = InteractionModel()
    fv = _fv({"a": 3.0, "b": 5.0})  # value = 15; baseline (empty) = 0
    expl = await PermutationExplainer(samples=16, seed=7).explain(model, fv)
    assert sum(expl.values()) == pytest.approx(15.0)


async def test_deterministic_given_seed():
    model = InteractionModel()
    fv = _fv({"a": 3.0, "b": 5.0, "c": 2.0})
    a = await PermutationExplainer(samples=8, seed=42).explain(model, fv)
    b = await PermutationExplainer(samples=8, seed=42).explain(model, fv)
    assert a == b


async def test_empty_feature_vector_explains_to_empty():
    model = LinearModel({"a": 1.0})
    expl = await PermutationExplainer().explain(model, _fv({}))
    assert expl == {}


async def test_baseline_reference_shifts_attribution():
    # With baseline a=1.0, the linear Shapley value is w_a·(x_a − baseline).
    model = LinearModel({"a": 1.5})
    fv = _fv({"a": 2.0})
    baseline = {"a": FeatureValue(scalar=1.0)}
    expl = await PermutationExplainer(samples=4, seed=1, baseline=baseline).explain(model, fv)
    assert expl["a"] == pytest.approx(1.5)  # 1.5 * (2 - 1)


def test_samples_must_be_positive():
    with pytest.raises(ValueError):
        PermutationExplainer(samples=0)


# --- ExplainingModel decorator ----------------------------------------


async def test_explaining_model_populates_explanation():
    inner = LinearModel({"a": 2.0, "b": 1.0})
    model = ExplainingModel(inner, PermutationExplainer(samples=4, seed=1))
    assert model.model_id == "lin@1"
    pred = await model.predict(_fv({"a": 3.0, "b": 4.0}))
    assert pred.value == pytest.approx(10.0)
    assert pred.explanation["a"] == pytest.approx(6.0)
    assert pred.explanation["b"] == pytest.approx(4.0)


async def test_explaining_model_skips_degraded_predictions():
    class DegradedModel:
        model_id = "deg@1"

        async def predict(self, fv):
            return PredictionEnvelope(
                subject_id=fv.subject_id, model=self.model_id, value=0.0,
                mode=PredictionMode.PREDICTION_MODE_DEGRADED, degraded_reason="x",
            )

    model = ExplainingModel(DegradedModel(), PermutationExplainer(samples=4))
    pred = await model.predict(_fv({"a": 1.0}))
    assert len(pred.explanation) == 0  # not explained


async def test_explaining_model_is_best_effort_on_explainer_failure():
    # StrictModel raises on the perturbed (partial) vectors the explainer
    # builds ⇒ explanation fails, but the real prediction still returns.
    model = ExplainingModel(StrictModel(), PermutationExplainer(samples=4))
    pred = await model.predict(_fv({"a": 1.0, "b": 2.0}))
    assert pred.value == pytest.approx(1.0)
    assert len(pred.explanation) == 0


def test_explaining_model_rejects_nil_collaborators():
    with pytest.raises(ValueError):
        ExplainingModel(None, PermutationExplainer())  # type: ignore[arg-type]
    with pytest.raises(ValueError):
        ExplainingModel(LinearModel({}), None)  # type: ignore[arg-type]
