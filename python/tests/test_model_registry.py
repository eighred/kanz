"""PRED-09 model registry tests + MLOPS-01a validation gate."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from inference.v1.feature_vector_pb2 import FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.registry import (
    ModelMetadata,
    Registry,
    ValidationError,
    ValidationRecord,
)


# A trivial Model that satisfies the streaming.worker.Model protocol
# structurally — `model_id` attr + async `predict`. The registry
# never calls predict in these tests; the protocol presence is what
# matters.
class _StubModel:
    def __init__(self, model_id: str = "stub@0"):
        self.model_id = model_id

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        return PredictionEnvelope(
            subject_id=fv.subject_id,
            model=self.model_id,
            value=0.0,
            mode=PredictionMode.PREDICTION_MODE_NORMAL,
        )


def _meta(
    model_id: str = "vol-forecast@1.4.2",
    feature_set_ref: str = "equity-momentum:7",
    confidence_threshold: float = 0.7,
    artifact_uri: str = "s3://kanz-models/vol-forecast/1.4.2/model.pkl",
) -> ModelMetadata:
    return ModelMetadata(
        model_id=model_id,
        feature_set_ref=feature_set_ref,
        confidence_threshold=confidence_threshold,
        artifact_uri=artifact_uri,
    )


def _valid(
    model_id: str,
    *,
    passed: bool = True,
    expires_in: timedelta = timedelta(days=365),
) -> ValidationRecord:
    """A validation record for the primary gate — passing and current by
    default."""
    now = datetime.now(timezone.utc)
    return ValidationRecord(
        model_id=model_id,
        validated_at=now,
        expires_at=now + expires_in,
        passed=passed,
    )


def _register_primary(r: Registry, m: ModelMetadata, model=None):
    """Record a passing validation then register ``m`` primary — the
    common path now that the MLOPS-01a gate requires evidence. Returns
    the registered model instance."""
    model = model or _StubModel(m.model_id)
    r.record_validation(_valid(m.model_id))
    r.register(m, model)
    return model


# --- Register / lookup ------------------------------------------------


def test_register_and_get_by_id_round_trip():
    r = Registry()
    m = _meta()
    model = _register_primary(r, m)
    got = r.get_by_id(m.model_id)
    assert got is not None
    md, mdl = got
    assert md == m
    assert mdl is model


def test_get_by_id_miss_returns_none():
    r = Registry()
    assert r.get_by_id("not-here@0") is None


def test_primary_for_feature_set_returns_primary():
    r = Registry()
    m = _meta()
    _register_primary(r, m)
    got = r.primary_for_feature_set(m.feature_set_ref)
    assert got is not None
    assert got[0].model_id == m.model_id


def test_primary_for_feature_set_miss_returns_none():
    r = Registry()
    assert r.primary_for_feature_set("equity-momentum:7") is None


def test_register_with_same_id_replaces_entry():
    r = Registry()
    m = _meta()
    first = _StubModel(m.model_id)
    second = _StubModel(m.model_id)
    r.record_validation(_valid(m.model_id))
    r.register(m, first)
    r.register(m, second)  # same model_id, replaces
    _, got_model = r.get_by_id(m.model_id)
    assert got_model is second


def test_register_new_primary_displaces_previous_primary():
    # Two models for the same feature_set_ref, both registered
    # primary — the second wins (latest-wins primary).
    r = Registry()
    a = _meta(model_id="vol-forecast@1.0.0")
    b = _meta(model_id="vol-forecast@1.1.0")
    _register_primary(r, a)
    _register_primary(r, b)
    md, _ = r.primary_for_feature_set(a.feature_set_ref)
    assert md.model_id == "vol-forecast@1.1.0"


# --- Shadows ----------------------------------------------------------


def test_shadows_collected_per_feature_set():
    r = Registry()
    primary = _meta(model_id="vol-forecast@1.4.2")
    shadow1 = _meta(model_id="vol-forecast@1.5.0-rc1")
    shadow2 = _meta(model_id="vol-forecast@1.5.0-rc2")

    _register_primary(r, primary)
    r.register(shadow1, _StubModel(shadow1.model_id), primary=False)
    r.register(shadow2, _StubModel(shadow2.model_id), primary=False)

    shadows = r.shadows_for_feature_set(primary.feature_set_ref)
    assert len(shadows) == 2
    ids = {md.model_id for md, _ in shadows}
    assert ids == {"vol-forecast@1.5.0-rc1", "vol-forecast@1.5.0-rc2"}

    # Primary is NOT in the shadow list.
    primary_md, _ = r.primary_for_feature_set(primary.feature_set_ref)
    assert primary_md.model_id == "vol-forecast@1.4.2"


def test_shadows_for_unknown_feature_set_is_empty():
    r = Registry()
    assert r.shadows_for_feature_set("unknown:0") == []


def test_promote_shadow_to_primary_via_reregister():
    # Demoting a primary back to shadow and promoting a previous
    # shadow is the rollout pattern. Verifying it works as expected
    # via the public API (re-register with primary=True/False).
    r = Registry()
    a = _meta(model_id="vol-forecast@1.0.0")
    b = _meta(model_id="vol-forecast@1.1.0")
    _register_primary(r, a)                                # primary
    r.register(b, _StubModel(b.model_id), primary=False)   # shadow

    # Promote b to primary; a becomes... whatever the caller wants.
    # Here, we re-register a as shadow. b's promotion needs validation
    # evidence (the MLOPS-01a gate) — the shadow earned it during eval.
    r.record_validation(_valid(b.model_id))
    r.register(b, _StubModel(b.model_id), primary=True)
    r.register(a, _StubModel(a.model_id), primary=False)

    primary_md, _ = r.primary_for_feature_set(a.feature_set_ref)
    assert primary_md.model_id == "vol-forecast@1.1.0"
    shadow_ids = {md.model_id for md, _ in r.shadows_for_feature_set(a.feature_set_ref)}
    assert shadow_ids == {"vol-forecast@1.0.0"}


# --- list_models + unregister ----------------------------------------


def test_list_models_returns_sorted():
    r = Registry()
    for mid in ["zeta@1", "alpha@1", "mu@1"]:
        _register_primary(r, _meta(model_id=mid))
    listed = [md.model_id for md in r.list_models()]
    assert listed == ["alpha@1", "mu@1", "zeta@1"]


def test_unregister_removes_from_all_indexes():
    r = Registry()
    primary = _meta(model_id="vol-forecast@1.4.2")
    shadow = _meta(model_id="vol-forecast@1.5.0-rc1")
    _register_primary(r, primary)
    r.register(shadow, _StubModel(shadow.model_id), primary=False)

    assert r.unregister("vol-forecast@1.4.2") is True
    assert r.get_by_id("vol-forecast@1.4.2") is None
    assert r.primary_for_feature_set(primary.feature_set_ref) is None
    # Shadow survives.
    assert len(r.shadows_for_feature_set(primary.feature_set_ref)) == 1


def test_unregister_unknown_returns_false():
    r = Registry()
    assert r.unregister("not-here@0") is False


# --- Validation -------------------------------------------------------


def test_register_rejects_empty_model_id():
    r = Registry()
    m = _meta(model_id="")
    with pytest.raises(ValueError):
        r.register(m, _StubModel())


def test_register_rejects_model_id_without_at_sign():
    r = Registry()
    m = _meta(model_id="vol-forecast-1.4.2")  # missing @
    with pytest.raises(ValueError):
        r.register(m, _StubModel())


def test_register_rejects_feature_set_ref_without_colon():
    r = Registry()
    m = _meta(feature_set_ref="equity-momentum-7")  # missing :
    with pytest.raises(ValueError):
        r.register(m, _StubModel())


def test_register_rejects_nil_model():
    r = Registry()
    with pytest.raises(ValueError):
        r.register(_meta(), None)  # type: ignore[arg-type]


def test_model_metadata_is_hashable():
    # Frozen dataclass — should be usable as a dict key.
    m = _meta()
    d = {m: "value"}
    assert d[_meta()] == "value"  # identical fields ⇒ equal hash


# --- MLOPS-01a validation gate ---------------------------------------


def test_primary_without_validation_is_blocked():
    r = Registry()
    m = _meta()
    with pytest.raises(ValidationError):
        r.register(m, _StubModel(m.model_id))  # no recorded validation
    # Nothing was registered — the gate runs before any mutation.
    assert r.get_by_id(m.model_id) is None
    assert r.primary_for_feature_set(m.feature_set_ref) is None


def test_validation_error_is_a_value_error():
    # Callers that already treat a rejected register as ValueError keep working.
    assert issubclass(ValidationError, ValueError)


def test_primary_with_expired_validation_is_blocked():
    r = Registry()
    m = _meta()
    r.record_validation(_valid(m.model_id, expires_in=timedelta(seconds=-1)))  # already lapsed
    with pytest.raises(ValidationError):
        r.register(m, _StubModel(m.model_id))


def test_primary_with_failed_validation_is_blocked():
    r = Registry()
    m = _meta()
    r.record_validation(_valid(m.model_id, passed=False))
    with pytest.raises(ValidationError):
        r.register(m, _StubModel(m.model_id))


def test_primary_with_valid_validation_succeeds():
    r = Registry()
    m = _meta()
    r.record_validation(_valid(m.model_id))
    r.register(m, _StubModel(m.model_id))  # no raise
    assert r.primary_for_feature_set(m.feature_set_ref)[0].model_id == m.model_id


def test_shadow_registration_is_exempt_from_the_gate():
    # A shadow is under evaluation, not serving — no validation required.
    r = Registry()
    m = _meta()
    r.register(m, _StubModel(m.model_id), primary=False)  # no raise
    assert len(r.shadows_for_feature_set(m.feature_set_ref)) == 1


def test_validation_a_models_id_only_gates_that_model():
    # A validation for a different model does not satisfy the gate.
    r = Registry()
    m = _meta(model_id="vol-forecast@2.0.0")
    r.record_validation(_valid("some-other@1.0.0"))
    with pytest.raises(ValidationError):
        r.register(m, _StubModel(m.model_id))


def test_record_validation_latest_wins():
    r = Registry()
    mid = "vol-forecast@1.4.2"
    r.record_validation(_valid(mid, passed=True))
    r.record_validation(_valid(mid, passed=False))  # revalidation failed
    assert r.validation_for(mid).passed is False
    with pytest.raises(ValidationError):
        r.register(_meta(model_id=mid), _StubModel(mid))


def test_record_validation_rejects_empty_model_id():
    r = Registry()
    with pytest.raises(ValueError):
        r.record_validation(_valid(""))


def test_validation_for_miss_returns_none():
    r = Registry()
    assert r.validation_for("not-here@0") is None


def test_gate_uses_injected_clock_for_expiry():
    # Pin "now" so the expiry boundary is deterministic.
    now = datetime(2026, 6, 21, 12, 0, 0, tzinfo=timezone.utc)
    r = Registry(clock=lambda: now)
    m = _meta()
    # Expires exactly one second after the pinned now ⇒ still valid.
    r.record_validation(
        ValidationRecord(
            model_id=m.model_id,
            validated_at=now - timedelta(days=1),
            expires_at=now + timedelta(seconds=1),
        )
    )
    r.register(m, _StubModel(m.model_id))  # no raise
    assert r.get_by_id(m.model_id) is not None


def test_reregister_primary_rechecks_gate_after_expiry():
    # Hot-reloading a primary re-runs the gate; an expired validation blocks it.
    clock = {"now": datetime(2026, 6, 21, 12, 0, 0, tzinfo=timezone.utc)}
    r = Registry(clock=lambda: clock["now"])
    m = _meta()
    r.record_validation(
        ValidationRecord(
            model_id=m.model_id,
            validated_at=clock["now"],
            expires_at=clock["now"] + timedelta(days=30),
        )
    )
    r.register(m, _StubModel(m.model_id))  # ok now

    clock["now"] = clock["now"] + timedelta(days=31)  # validation has since lapsed
    with pytest.raises(ValidationError):
        r.register(m, _StubModel(m.model_id))  # re-register blocked
