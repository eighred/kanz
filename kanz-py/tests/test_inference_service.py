"""AI-M1 — the inference service that was built and never ran.

kanz_inference had a gRPC servicer, a streaming worker, a point-in-time feature
store, a permutation-Shapley explainer, a model registry with a promotion gate,
shadow execution, validation and governance — and NO ENTRYPOINT, NO DOCKERFILE,
NO CONCRETE MODEL. Every ``Model`` outside the tests was a ``typing.Protocol``,
and on the Go side ``internal/prediction`` had zero importers outside its own
tests. Nothing served ``InferenceService.Predict``; nothing called it.

So the platform's entire AI capability was an exceptionally well-specified
contract with nothing behind it. These tests are what make it real: one concrete
model, composed through the machinery that already existed, refusing to serve
when it has nothing to serve.
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from inference.v1.feature_vector_pb2 import FeatureValue, FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.models.linear import LinearModel, ModelSpec
from kanz_inference.registry.registry import Registry, ValidationRecord
from kanz_inference.service import NoPrimaryModelError, RegistryRouter, build_registry, require_primary

FEATURE_SET = "portfolio-risk:1"


def _spec(**over) -> ModelSpec:
    base = dict(
        model_id="portfolio-risk@1.0.0",
        feature_set_ref=FEATURE_SET,
        confidence_threshold=0.5,
        weights={"gross_exposure": 0.4, "var99": -0.9},
        bias=0.1,
        validated_at=datetime.now(timezone.utc) - timedelta(days=1),
        expires_at=datetime.now(timezone.utc) + timedelta(days=30),
        validation_passed=True,
    )
    base.update(over)
    return ModelSpec(**base)


def _fv(**scalars: float) -> FeatureVector:
    fv = FeatureVector(subject_id="fund-alpha", feature_set_ref=FEATURE_SET)
    for name, x in scalars.items():
        fv.values[name].CopyFrom(FeatureValue(scalar=x))
    return fv


# --- The model is real -------------------------------------------------


async def test_a_prediction_carries_a_value_and_an_honest_confidence():
    """The whole point of the contract: a prediction is not a bare number. It
    says how much it trusts itself, and PredictionEnvelope.confidence is what
    every downstream consumer gates on."""
    model = LinearModel(_spec())
    p = await model.predict(_fv(gross_exposure=2.0, var99=1.0))

    # 0.4*2 + -0.9*1 + 0.1 = 0.0
    assert p.value == pytest.approx(0.0)
    assert p.subject_id == "fund-alpha"
    assert p.model == "portfolio-risk@1.0.0"
    assert 0.0 <= p.confidence <= 1.0


async def test_a_prediction_below_the_models_own_threshold_is_DEGRADED():
    """PRED-02: the confidence threshold is the MODEL's contract, not a global
    constant. A prediction the model does not trust must SAY SO — a consumer
    that cannot tell a confident call from a coin flip will act on both."""
    model = LinearModel(_spec(confidence_threshold=0.99))
    p = await model.predict(_fv(gross_exposure=0.0, var99=0.0))

    assert p.mode == PredictionMode.PREDICTION_MODE_DEGRADED
    assert p.degraded_reason != ""


async def test_the_model_declares_what_it_could_not_see():
    """A feature the model was trained on and did NOT receive is not a zero —
    it is a hole in the input. Scoring it as zero would silently produce a
    confident prediction from half a feature vector."""
    model = LinearModel(_spec())
    p = await model.predict(_fv(gross_exposure=2.0))  # var99 MISSING

    assert p.mode == PredictionMode.PREDICTION_MODE_DEGRADED
    assert "var99" in p.degraded_reason


# --- The registry gate is real ----------------------------------------


async def test_an_UNVALIDATED_model_cannot_serve():
    """MLOPS-01a / SR 11-7: no model serves production without recorded,
    current validation. The registry already enforces this; nothing had ever
    driven it from a composition root."""
    registry = Registry()
    with pytest.raises(Exception):  # ValidationError from the primary gate
        registry.register(_spec().metadata(), LinearModel(_spec()), primary=True)


async def test_an_EXPIRED_validation_cannot_serve():
    """A validation that has lapsed is not a validation. Model risk is a
    revalidation cadence, not a one-time sign-off."""
    stale = _spec(
        validated_at=datetime.now(timezone.utc) - timedelta(days=400),
        expires_at=datetime.now(timezone.utc) - timedelta(days=1),
    )
    with pytest.raises(Exception):
        build_registry([stale])


async def test_a_validated_model_serves():
    registry = build_registry([_spec()])
    entry = registry.primary_for_feature_set(FEATURE_SET)
    assert entry is not None
    assert entry[0].model_id == "portfolio-risk@1.0.0"


# --- The service refuses to serve nothing ------------------------------


async def test_the_service_REFUSES_TO_START_with_no_primary_model():
    """The fail-closed posture this platform applies everywhere: the gateway
    refuses to start unauthenticated (SEC-M1), webhook-ingest refuses to start
    without a replay defence (EXEC-M17/M22), tv-sync refuses to start without a
    fact log (EXEC-M21).

    A prediction service serving nothing is worse than one that is down: it
    reports healthy, and it answers."""
    with pytest.raises(NoPrimaryModelError):
        require_primary(Registry())


async def test_a_request_for_an_UNKNOWN_feature_set_is_DEGRADED_not_a_crash():
    """At REQUEST time the posture inverts: a feature set with no primary must
    come back as a DEGRADED envelope, never an exception and never a silent
    zero. The Go client's circuit breaker is built to consume exactly this —
    'I could not predict' is an answer; a black hole is not."""
    router = RegistryRouter(build_registry([_spec()]))
    fv = FeatureVector(subject_id="fund-alpha", feature_set_ref="something-else:9")

    p = await router.predict(fv)

    assert isinstance(p, PredictionEnvelope)
    assert p.mode == PredictionMode.PREDICTION_MODE_DEGRADED
    assert p.degraded_reason != ""
    assert p.confidence == 0.0  # unknown confidence IS no confidence


async def test_the_router_serves_the_primary_for_the_feature_set():
    router = RegistryRouter(build_registry([_spec()]))
    # value = 0.4*5 - 0.9*1 + 0.1 = 1.2 ⇒ confidence 0.537, clear of the 0.5 threshold.
    p = await router.predict(_fv(gross_exposure=5.0, var99=1.0))

    assert p.model == "portfolio-risk@1.0.0"
    assert p.mode == PredictionMode.PREDICTION_MODE_NORMAL


# --- Explainability is wired, not aspirational -------------------------


async def test_the_served_prediction_EXPLAINS_ITSELF():
    """The XAI payload the brief asks for already exists in the contract
    (PredictionEnvelope.explanation) and the explainer already exists
    (permutation Shapley). They had simply never been connected to a model.

    A risk engine that acts on a number it cannot attribute is a risk engine
    nobody can audit."""
    router = RegistryRouter(build_registry([_spec()]), explain=True)
    p = await router.predict(_fv(gross_exposure=5.0, var99=1.0))

    assert p.explanation, "the prediction carried no explanation"
    # Linear model: the Shapley contribution is exactly w * x, which is why the
    # explainer's own tests are written against one.
    assert p.explanation["gross_exposure"] == pytest.approx(2.0)   # 0.4 * 5
    assert p.explanation["var99"] == pytest.approx(-0.9)           # -0.9 * 1
