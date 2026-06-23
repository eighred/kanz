"""DEBT-02b — CoordinatedRegistry: model registrations span replicas via the
platform.model topic, so the fleet converges instead of each worker holding
isolated process-local state."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from inference.v1.feature_vector_pb2 import FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.registry import (
    CoordinatedRegistry,
    ModelMetadata,
    Registry,
    RegistryEvent,
    ValidationRecord,
)


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


class _StubLoader:
    """Rematerializes a Model from metadata — the apply-side seam."""

    def __init__(self) -> None:
        self.loaded: list[str] = []

    async def load(self, metadata: ModelMetadata) -> _StubModel:
        self.loaded.append(metadata.model_id)
        return _StubModel(metadata.model_id)


class _FakeTopic:
    """Stand-in for platform.model: collects published events (as wire dicts, to
    exercise serialization) for manual delivery to peers in the tests."""

    def __init__(self) -> None:
        self.log: list[dict] = []

    async def publish(self, event: RegistryEvent) -> None:
        self.log.append(event.to_dict())

    def events(self) -> list[RegistryEvent]:
        return [RegistryEvent.from_dict(d) for d in self.log]


def _meta(model_id="vol-forecast@1.4.2", fs="equity-momentum:7", ct=0.7):
    return ModelMetadata(
        model_id=model_id,
        feature_set_ref=fs,
        confidence_threshold=ct,
        artifact_uri=f"s3://kanz-models/{model_id}/model.pkl",
    )


def _valid(model_id, *, passed=True, expires_in=timedelta(days=365)):
    now = datetime.now(timezone.utc)
    return ValidationRecord(
        model_id=model_id,
        validated_at=now,
        expires_at=now + expires_in,
        passed=passed,
    )


async def _deliver(topic: _FakeTopic, *targets: CoordinatedRegistry) -> None:
    """Replay every published event into each target's apply (the consumer)."""
    for event in topic.events():
        for t in targets:
            await t.apply(event)


@pytest.mark.asyncio
async def test_register_converges_across_replicas():
    topic = _FakeTopic()
    loader_b = _StubLoader()
    pod_a = CoordinatedRegistry(
        local=Registry(), publisher=topic, loader=_StubLoader(), origin="pod-a"
    )
    pod_b = CoordinatedRegistry(
        local=Registry(), publisher=topic, loader=loader_b, origin="pod-b"
    )

    meta = _meta()
    await pod_a.record_validation(_valid(meta.model_id))
    await pod_a.register(meta, _StubModel(meta.model_id), primary=True)

    # pod_b has not seen the model yet — only pod_a's local registry has it.
    assert pod_b.local.primary_for_feature_set(meta.feature_set_ref) is None

    await _deliver(topic, pod_b)

    # Now pod_b serves the same primary, rematerialized via its loader.
    served = pod_b.local.primary_for_feature_set(meta.feature_set_ref)
    assert served is not None and served[0].model_id == meta.model_id
    assert loader_b.loaded == [meta.model_id]


@pytest.mark.asyncio
async def test_apply_skips_self_origin():
    topic = _FakeTopic()
    loader = _StubLoader()
    pod = CoordinatedRegistry(
        local=Registry(), publisher=topic, loader=loader, origin="pod-a"
    )
    meta = _meta()
    await pod.record_validation(_valid(meta.model_id))
    await pod.register(meta, _StubModel(meta.model_id), primary=True)

    # Applying its own published events must be a no-op (no re-load).
    await _deliver(topic, pod)
    assert loader.loaded == []  # never rematerialized its own model


@pytest.mark.asyncio
async def test_unregister_propagates():
    topic = _FakeTopic()
    pod_a = CoordinatedRegistry(
        local=Registry(), publisher=topic, loader=_StubLoader(), origin="pod-a"
    )
    pod_b = CoordinatedRegistry(
        local=Registry(), publisher=topic, loader=_StubLoader(), origin="pod-b"
    )
    meta = _meta()
    await pod_a.record_validation(_valid(meta.model_id))
    await pod_a.register(meta, _StubModel(meta.model_id), primary=True)
    await _deliver(topic, pod_b)
    assert pod_b.local.get_by_id(meta.model_id) is not None

    await pod_a.unregister(meta.model_id)
    await _deliver(topic, pod_b)
    assert pod_b.local.get_by_id(meta.model_id) is None


@pytest.mark.asyncio
async def test_unregister_absent_does_not_publish():
    topic = _FakeTopic()
    pod = CoordinatedRegistry(
        local=Registry(), publisher=topic, loader=_StubLoader(), origin="pod-a"
    )
    removed = await pod.unregister("nope@1")
    assert removed is False
    assert topic.log == []  # no-op produced no event


def test_event_serialization_round_trip():
    now = datetime.now(timezone.utc)
    ev = RegistryEvent(
        op="record_validation",
        origin="pod-a",
        model_id="m@1",
        validated_at=now,
        expires_at=now + timedelta(days=1),
        passed=True,
        report_uri="s3://r",
    )
    back = RegistryEvent.from_dict(ev.to_dict())
    assert back == ev
