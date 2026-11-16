"""MLOPS-01e champion/challenger promotion tests."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from inference.v1.feature_vector_pb2 import FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.registry import ModelMetadata, Registry, ValidationRecord
from kanz_inference.shadow import ShadowObserver
from kanz_inference.governance import (
    Promoter,
    ShadowComparisonStore,
)

FS = "equity-momentum:7"


class _Stub:
    def __init__(self, model_id: str):
        self.model_id = model_id

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        return PredictionEnvelope(
            subject_id=fv.subject_id, model=self.model_id, value=0.0,
            mode=PredictionMode.PREDICTION_MODE_NORMAL,
        )


def _meta(model_id: str) -> ModelMetadata:
    return ModelMetadata(
        model_id=model_id, feature_set_ref=FS, confidence_threshold=0.7,
        artifact_uri=f"s3://x/{model_id}",
    )


def _valid(model_id: str) -> ValidationRecord:
    now = datetime.now(timezone.utc)
    return ValidationRecord(
        model_id=model_id, validated_at=now, expires_at=now + timedelta(days=365),
    )


def _registry_with(champion: str, *challengers: str, validate=()) -> Registry:
    r = Registry()
    r.record_validation(_valid(champion))
    r.register(_meta(champion), _Stub(champion))  # primary
    for c in challengers:
        if c in validate:
            r.record_validation(_valid(c))
        r.register(_meta(c), _Stub(c), primary=False)  # shadow
    return r


async def _feed(store, subject, primary_id, pv, challenger_id, sv, actual=None):
    fv = FeatureVector(subject_id=subject, feature_set_ref=FS)
    await store.observe(
        fv,
        _meta(primary_id), PredictionEnvelope(value=pv),
        _meta(challenger_id), PredictionEnvelope(value=sv),
    )
    if actual is not None:
        store.record_outcome(subject, actual)


async def _feed_n(store, n, primary_id, pv, challenger_id, sv, actual, *, scored=None):
    scored = n if scored is None else scored
    for i in range(n):
        a = actual if i < scored else None
        await _feed(store, f"S{i}", primary_id, pv, challenger_id, sv, a)


# --- Store -------------------------------------------------------------


def test_store_is_a_shadow_observer():
    assert isinstance(ShadowComparisonStore(), ShadowObserver)


async def test_store_accumulates_behavioral_and_accuracy():
    store = ShadowComparisonStore()
    # champion predicts 2.0, challenger 1.0, actual 1.0 ⇒ champion err 1, challenger err 0.
    await _feed_n(store, 4, "champ@1", 2.0, "chal@1", 1.0, 1.0)
    s = store.stats("champ@1", "chal@1")
    assert s.observations == 4
    assert s.scored == 4
    assert s.mean_abs_divergence == pytest.approx(1.0)  # |1-2|
    assert s.primary_rmse == pytest.approx(1.0)
    assert s.challenger_rmse == pytest.approx(0.0)


async def test_disagreement_rate_counts_opposite_signs():
    store = ShadowComparisonStore()
    await _feed(store, "A", "champ@1", 1.0, "chal@1", -1.0)  # opposite ⇒ disagree
    await _feed(store, "B", "champ@1", 1.0, "chal@1", 0.5)   # same sign
    s = store.stats("champ@1", "chal@1")
    assert s.disagreement_rate == pytest.approx(0.5)


def test_record_outcome_unknown_subject_is_noop():
    store = ShadowComparisonStore()
    store.record_outcome("never-seen", 1.0)  # no raise
    assert store.stats("a@1", "b@1").scored == 0


# --- Promotion: happy path --------------------------------------------


async def test_promotes_validated_outperforming_challenger():
    r = _registry_with("champ@1", "chal@1", validate=("chal@1",))
    store = ShadowComparisonStore()
    await _feed_n(store, 5, "champ@1", 2.0, "chal@1", 1.0, 1.0)  # challenger far better

    p = Promoter(r, store, min_observations=3, min_scored=3)
    decision = p.evaluate(FS)

    assert decision.promoted == "chal@1"
    assert decision.previous_primary == "champ@1"
    # Registry now serves the challenger; old champion demoted to shadow.
    assert r.primary_for_feature_set(FS)[0].model_id == "chal@1"
    assert {m.model_id for m, _ in r.shadows_for_feature_set(FS)} == {"champ@1"}


async def test_best_of_several_challengers_is_chosen():
    r = _registry_with("champ@1", "chal-a@1", "chal-b@1", validate=("chal-a@1", "chal-b@1"))
    store = ShadowComparisonStore()
    # champion err 1.0; A err 0.5; B err 0.2 ⇒ B wins.
    await _feed_n(store, 5, "champ@1", 2.0, "chal-a@1", 1.5, 1.0)
    await _feed_n(store, 5, "champ@1", 2.0, "chal-b@1", 1.2, 1.0)

    decision = Promoter(r, store, min_observations=3, min_scored=3).evaluate(FS)
    assert decision.promoted == "chal-b@1"


# --- Promotion: each gate blocks independently ------------------------


async def test_blocked_by_insufficient_soak():
    r = _registry_with("champ@1", "chal@1", validate=("chal@1",))
    store = ShadowComparisonStore()
    await _feed_n(store, 2, "champ@1", 2.0, "chal@1", 1.0, 1.0)  # only 2 obs
    decision = Promoter(r, store, min_observations=3, min_scored=1).evaluate(FS)
    assert decision.promoted is None
    assert any("soak" in reason for reason in decision.reasons)
    assert r.primary_for_feature_set(FS)[0].model_id == "champ@1"  # unchanged


async def test_blocked_by_insufficient_scored_outcomes():
    r = _registry_with("champ@1", "chal@1", validate=("chal@1",))
    store = ShadowComparisonStore()
    # 5 observations but only 2 joined to outcomes.
    await _feed_n(store, 5, "champ@1", 2.0, "chal@1", 1.0, 1.0, scored=2)
    decision = Promoter(r, store, min_observations=3, min_scored=3).evaluate(FS)
    assert decision.promoted is None
    assert any("scored" in reason for reason in decision.reasons)


async def test_blocked_without_validation():
    # Challenger is a shadow (allowed unvalidated) but cannot be promoted.
    r = _registry_with("champ@1", "chal@1")  # chal NOT validated
    store = ShadowComparisonStore()
    await _feed_n(store, 5, "champ@1", 2.0, "chal@1", 1.0, 1.0)
    decision = Promoter(r, store, min_observations=3, min_scored=3).evaluate(FS)
    assert decision.promoted is None
    assert any("validation" in reason for reason in decision.reasons)


async def test_blocked_when_not_outperforming():
    r = _registry_with("champ@1", "chal@1", validate=("chal@1",))
    store = ShadowComparisonStore()
    # Identical predictions ⇒ identical RMSE ⇒ no out-performance.
    await _feed_n(store, 5, "champ@1", 2.0, "chal@1", 2.0, 1.0)
    decision = Promoter(r, store, min_observations=3, min_scored=3).evaluate(FS)
    assert decision.promoted is None
    assert any("beat" in reason for reason in decision.reasons)


async def test_marginal_win_below_margin_is_blocked():
    r = _registry_with("champ@1", "chal@1", validate=("chal@1",))
    store = ShadowComparisonStore()
    # champion err 1.0, challenger err 0.98 ⇒ 2% better, below a 10% margin.
    await _feed_n(store, 5, "champ@1", 2.0, "chal@1", 1.98, 1.0)
    decision = Promoter(
        r, store, min_observations=3, min_scored=3, improvement_margin=0.10
    ).evaluate(FS)
    assert decision.promoted is None


async def test_no_primary_registered():
    decision = Promoter(Registry(), ShadowComparisonStore()).evaluate(FS)
    assert decision.promoted is None
    assert any("no primary" in reason for reason in decision.reasons)


# --- Constructor guards -----------------------------------------------


def test_constructor_guards():
    with pytest.raises(ValueError):
        Promoter(None, ShadowComparisonStore())  # type: ignore[arg-type]
    with pytest.raises(ValueError):
        Promoter(Registry(), None)  # type: ignore[arg-type]
    with pytest.raises(ValueError):
        Promoter(Registry(), ShadowComparisonStore(), improvement_margin=1.0)
