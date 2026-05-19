"""PRED-09 model registry tests."""

from __future__ import annotations

import pytest

from inference.v1.feature_vector_pb2 import FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.registry import ModelMetadata, Registry


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


# --- Register / lookup ------------------------------------------------


def test_register_and_get_by_id_round_trip():
    r = Registry()
    m = _meta()
    model = _StubModel(m.model_id)
    r.register(m, model)
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
    r.register(m, _StubModel(m.model_id))
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
    r.register(a, _StubModel(a.model_id))
    r.register(b, _StubModel(b.model_id))
    md, _ = r.primary_for_feature_set(a.feature_set_ref)
    assert md.model_id == "vol-forecast@1.1.0"


# --- Shadows ----------------------------------------------------------


def test_shadows_collected_per_feature_set():
    r = Registry()
    primary = _meta(model_id="vol-forecast@1.4.2")
    shadow1 = _meta(model_id="vol-forecast@1.5.0-rc1")
    shadow2 = _meta(model_id="vol-forecast@1.5.0-rc2")

    r.register(primary, _StubModel(primary.model_id))
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
    r.register(a, _StubModel(a.model_id))                  # primary
    r.register(b, _StubModel(b.model_id), primary=False)   # shadow

    # Promote b to primary; a becomes... whatever the caller wants.
    # Here, we re-register a as shadow.
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
        m = _meta(model_id=mid)
        r.register(m, _StubModel(mid))
    listed = [md.model_id for md in r.list_models()]
    assert listed == ["alpha@1", "mu@1", "zeta@1"]


def test_unregister_removes_from_all_indexes():
    r = Registry()
    primary = _meta(model_id="vol-forecast@1.4.2")
    shadow = _meta(model_id="vol-forecast@1.5.0-rc1")
    r.register(primary, _StubModel(primary.model_id))
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
