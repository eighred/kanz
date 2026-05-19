"""PRED-04 streaming worker tests."""

from __future__ import annotations

from typing import Optional

import pytest
from google.protobuf.timestamp_pb2 import Timestamp

from envelope.v1.envelope_pb2 import Envelope
from inference.v1.feature_vector_pb2 import FeatureValue, FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.streaming import (
    LastKnownCache,
    PredictionPublisher,
    StreamingWorker,
)
from kanz_inference.streaming.worker import (
    REASON_INFERENCE_UNAVAILABLE,
    REASON_NO_CACHED_PREDICTION,
)


# --- Fakes ------------------------------------------------------------


class FakeSubscriber:
    """Satisfies the kanz_bus.Subscriber protocol; subscribe blocks
    in real life, but tests don't drive it (they call worker.handle
    directly)."""

    async def subscribe(self, subject, group, handler):
        raise NotImplementedError("tests drive handle directly")


class FakeModel:
    """Returns the configured prediction, or raises if exc is set."""

    def __init__(
        self,
        model_id: str = "test-model@1.0.0",
        prediction: Optional[PredictionEnvelope] = None,
        exc: Optional[Exception] = None,
    ):
        self.model_id = model_id
        self._prediction = prediction
        self._exc = exc
        self.calls: list[FeatureVector] = []

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        self.calls.append(fv)
        if self._exc is not None:
            raise self._exc
        return self._prediction


class FakePublisher:
    """Records every published prediction for inspection."""

    def __init__(self):
        self.published: list[tuple[Envelope, PredictionEnvelope]] = []

    async def publish_prediction(self, env: Envelope, prediction: PredictionEnvelope):
        self.published.append((env, prediction))


# --- Helpers ----------------------------------------------------------


def _make_envelope(event_id: str = "evt-1") -> Envelope:
    env = Envelope()
    env.event_id = event_id
    env.correlation_id = event_id
    return env


def _make_feature_vector(subject_id: str = "AAPL") -> FeatureVector:
    fv = FeatureVector()
    fv.subject_id = subject_id
    fv.feature_set_ref = "equity-momentum:7"
    fv.as_of.CopyFrom(Timestamp(seconds=1767225600))
    fv.values["return_5d"].CopyFrom(FeatureValue(scalar=0.025))
    return fv


def _normal_prediction(subject_id: str = "AAPL", value: float = 0.75) -> PredictionEnvelope:
    return PredictionEnvelope(
        subject_id=subject_id,
        model="test-model@1.0.0",
        value=value,
        confidence=0.9,
        mode=PredictionMode.PREDICTION_MODE_NORMAL,
        as_of=Timestamp(seconds=1767225600),
    )


# --- Tests ------------------------------------------------------------


def test_constructor_rejects_nil_args():
    with pytest.raises(ValueError):
        StreamingWorker(subscriber=None, model=FakeModel(), publisher=FakePublisher())
    with pytest.raises(ValueError):
        StreamingWorker(subscriber=FakeSubscriber(), model=None, publisher=FakePublisher())
    with pytest.raises(ValueError):
        StreamingWorker(subscriber=FakeSubscriber(), model=FakeModel(), publisher=None)


async def test_handle_normal_path_publishes_prediction():
    model = FakeModel(prediction=_normal_prediction())
    pub = FakePublisher()
    worker = StreamingWorker(subscriber=FakeSubscriber(), model=model, publisher=pub)

    fv = _make_feature_vector()
    env = _make_envelope("evt-1")
    await worker.handle(env, fv.SerializeToString())

    assert len(model.calls) == 1
    assert model.calls[0].subject_id == "AAPL"
    assert len(pub.published) == 1
    out_env, out_pred = pub.published[0]
    assert out_env is env  # original envelope passed through for lineage
    assert out_pred.mode == PredictionMode.PREDICTION_MODE_NORMAL
    assert out_pred.value == pytest.approx(0.75)


async def test_handle_caches_only_normal_predictions():
    cache = LastKnownCache()
    model = FakeModel(prediction=_normal_prediction())
    pub = FakePublisher()
    worker = StreamingWorker(
        subscriber=FakeSubscriber(), model=model, publisher=pub, cache=cache
    )
    await worker.handle(_make_envelope(), _make_feature_vector().SerializeToString())
    assert cache.lookup("AAPL") is not None


async def test_handle_does_not_cache_degraded_predictions():
    # PRED-02 §1 — caching a DEGRADED prediction would let it serve
    # later requests as if it were fresh (trust laundering).
    cache = LastKnownCache()
    degraded = _normal_prediction()
    degraded.mode = PredictionMode.PREDICTION_MODE_DEGRADED
    degraded.degraded_reason = "low_confidence"
    model = FakeModel(prediction=degraded)
    pub = FakePublisher()
    worker = StreamingWorker(
        subscriber=FakeSubscriber(), model=model, publisher=pub, cache=cache
    )
    await worker.handle(_make_envelope(), _make_feature_vector().SerializeToString())
    assert cache.lookup("AAPL") is None


async def test_handle_model_exception_falls_back_to_cache_hit():
    # PRED-02 §2.1 — model unavailable, cache has prior NORMAL
    # value, return cached + DEGRADED + inference_unavailable.
    cache = LastKnownCache()
    cache.store("AAPL", _normal_prediction(value=1.23))

    model = FakeModel(exc=RuntimeError("model server down"))
    pub = FakePublisher()
    worker = StreamingWorker(
        subscriber=FakeSubscriber(), model=model, publisher=pub, cache=cache
    )
    await worker.handle(_make_envelope(), _make_feature_vector().SerializeToString())

    assert len(pub.published) == 1
    _, out = pub.published[0]
    assert out.mode == PredictionMode.PREDICTION_MODE_DEGRADED
    assert out.degraded_reason == REASON_INFERENCE_UNAVAILABLE
    assert out.value == pytest.approx(1.23)  # cached value preserved
    assert out.subject_id == "AAPL"


async def test_handle_model_exception_with_no_cache_returns_no_cached_prediction():
    # PRED-02 §2.1 — cold cache + model down ⇒ value=0 confidence=0
    # degraded_reason=no_cached_prediction.
    model = FakeModel(exc=RuntimeError("model server down"))
    pub = FakePublisher()
    worker = StreamingWorker(
        subscriber=FakeSubscriber(), model=model, publisher=pub
    )
    await worker.handle(_make_envelope(), _make_feature_vector().SerializeToString())

    _, out = pub.published[0]
    assert out.mode == PredictionMode.PREDICTION_MODE_DEGRADED
    assert out.degraded_reason == REASON_NO_CACHED_PREDICTION
    assert out.value == 0.0
    assert out.confidence == 0.0
    assert out.model == "test-model@1.0.0"  # carries the worker's model id


async def test_handle_lineage_envelope_id_threaded_into_prediction():
    # PRED-01's PredictionEnvelope.feature_vector_event_id back-points
    # to the FeatureVector event that produced it — verify the
    # original envelope's event_id is propagated through the fallback
    # path (the normal path lets the model set it).
    model = FakeModel(exc=RuntimeError("down"))
    pub = FakePublisher()
    worker = StreamingWorker(subscriber=FakeSubscriber(), model=model, publisher=pub)
    env = _make_envelope("evt-original-42")
    await worker.handle(env, _make_feature_vector().SerializeToString())
    _, out = pub.published[0]
    assert out.feature_vector_event_id == "evt-original-42"


async def test_handle_malformed_payload_raises():
    model = FakeModel(prediction=_normal_prediction())
    pub = FakePublisher()
    worker = StreamingWorker(subscriber=FakeSubscriber(), model=model, publisher=pub)
    with pytest.raises(Exception):
        await worker.handle(_make_envelope(), b"\xff\xff\xff\xff")
    # Model + publisher never called for unparseable input.
    assert len(model.calls) == 0
    assert len(pub.published) == 0


def test_last_known_cache_overwrite_replaces_previous():
    c = LastKnownCache()
    c.store("AAPL", _normal_prediction(value=1.0))
    c.store("AAPL", _normal_prediction(value=2.0))
    got = c.lookup("AAPL")
    assert got is not None
    assert got.value == pytest.approx(2.0)


def test_last_known_cache_miss_returns_none():
    c = LastKnownCache()
    assert c.lookup("UNKNOWN") is None


def test_last_known_cache_empty_subject_id_is_noop():
    c = LastKnownCache()
    c.store("", _normal_prediction())  # must not blow up or write phantom entry
    assert c.lookup("") is None
