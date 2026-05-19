"""PRED-08 interactive servicer + admission control tests."""

from __future__ import annotations

import asyncio

import pytest
from google.protobuf.timestamp_pb2 import Timestamp

from inference.v1.feature_vector_pb2 import FeatureValue, FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.interactive import InferenceServicer
from kanz_inference.interactive.servicer import (
    REASON_INFERENCE_UNAVAILABLE,
    REASON_POOL_SATURATED,
)


def _make_fv(subject: str = "AAPL") -> FeatureVector:
    fv = FeatureVector()
    fv.subject_id = subject
    fv.feature_set_ref = "equity-momentum:7"
    fv.as_of.CopyFrom(Timestamp(seconds=1767225600))
    fv.values["return_5d"].CopyFrom(FeatureValue(scalar=0.025))
    return fv


def _normal(subject: str, value: float = 0.75) -> PredictionEnvelope:
    return PredictionEnvelope(
        subject_id=subject,
        model="test-model@1.0.0",
        value=value,
        confidence=0.9,
        mode=PredictionMode.PREDICTION_MODE_NORMAL,
        as_of=Timestamp(seconds=1767225600),
    )


class FakeModel:
    """Configurable async model. ``hold`` blocks predict on an
    event so admission-control tests can pile up in-flight calls
    deterministically.
    """

    def __init__(
        self,
        model_id: str = "test-model@1.0.0",
        prediction: PredictionEnvelope | None = None,
        exc: Exception | None = None,
        hold: asyncio.Event | None = None,
    ):
        self.model_id = model_id
        self._prediction = prediction
        self._exc = exc
        self._hold = hold
        self.calls = 0

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        self.calls += 1
        if self._hold is not None:
            await self._hold.wait()
        if self._exc is not None:
            raise self._exc
        return self._prediction


# --- Constructor ------------------------------------------------------


def test_constructor_rejects_nil_model():
    with pytest.raises(ValueError):
        InferenceServicer(model=None)  # type: ignore[arg-type]


def test_constructor_rejects_non_positive_pool():
    model = FakeModel(prediction=_normal("AAPL"))
    with pytest.raises(ValueError):
        InferenceServicer(model=model, max_in_flight=0)
    with pytest.raises(ValueError):
        InferenceServicer(model=model, max_in_flight=-1)


# --- Predict happy path ----------------------------------------------


async def test_predict_normal_path_returns_model_response():
    model = FakeModel(prediction=_normal("AAPL", 0.73))
    servicer = InferenceServicer(model)
    got = await servicer.Predict(_make_fv("AAPL"), None)
    assert got.mode == PredictionMode.PREDICTION_MODE_NORMAL
    assert got.value == pytest.approx(0.73)
    assert model.calls == 1


async def test_predict_decrements_in_flight_after_success():
    model = FakeModel(prediction=_normal("AAPL"))
    servicer = InferenceServicer(model)
    assert servicer.in_flight == 0
    await servicer.Predict(_make_fv(), None)
    assert servicer.in_flight == 0


# --- Model exception → DEGRADED inference_unavailable -----------------


async def test_predict_model_exception_returns_degraded_unavailable():
    model = FakeModel(exc=RuntimeError("model server down"))
    servicer = InferenceServicer(model)
    got = await servicer.Predict(_make_fv("AAPL"), None)
    assert got.mode == PredictionMode.PREDICTION_MODE_DEGRADED
    assert got.degraded_reason == REASON_INFERENCE_UNAVAILABLE
    assert got.subject_id == "AAPL"
    assert got.model == "test-model@1.0.0"  # would-have-been model traced
    assert got.value == 0.0
    assert got.confidence == 0.0


async def test_predict_decrements_in_flight_after_exception():
    model = FakeModel(exc=RuntimeError("down"))
    servicer = InferenceServicer(model)
    await servicer.Predict(_make_fv(), None)
    assert servicer.in_flight == 0, "in_flight must decrement even on exception"


# --- Admission control ------------------------------------------------


async def test_predict_admission_rejects_when_pool_saturated():
    # Hold all max_in_flight slots, then attempt one more call.
    # Should be rejected immediately without invoking the model.
    hold = asyncio.Event()
    model = FakeModel(prediction=_normal("AAPL"), hold=hold)
    servicer = InferenceServicer(model, max_in_flight=2)

    # Two in-flight calls (will block on hold).
    t1 = asyncio.create_task(servicer.Predict(_make_fv("A"), None))
    t2 = asyncio.create_task(servicer.Predict(_make_fv("B"), None))
    # Let them start.
    await asyncio.sleep(0.01)
    assert servicer.in_flight == 2

    # Third call — pool is full, must reject immediately.
    rejected = await servicer.Predict(_make_fv("C"), None)
    assert rejected.mode == PredictionMode.PREDICTION_MODE_DEGRADED
    assert rejected.degraded_reason == REASON_POOL_SATURATED
    # Model.predict was NOT called for the rejected request — only
    # the two held calls have incremented model.calls.
    assert model.calls == 2

    # Release the held calls so the test cleans up.
    hold.set()
    await t1
    await t2
    assert servicer.in_flight == 0


async def test_predict_admission_recovers_after_slot_frees():
    hold = asyncio.Event()
    model = FakeModel(prediction=_normal("AAPL"), hold=hold)
    servicer = InferenceServicer(model, max_in_flight=1)

    # Fill the pool.
    t1 = asyncio.create_task(servicer.Predict(_make_fv("A"), None))
    await asyncio.sleep(0.01)
    rejected = await servicer.Predict(_make_fv("B"), None)
    assert rejected.degraded_reason == REASON_POOL_SATURATED

    # Release the held call; pool reopens.
    hold.set()
    await t1
    assert servicer.in_flight == 0

    # Next call admits.
    hold2 = asyncio.Event()
    hold2.set()
    model2 = FakeModel(prediction=_normal("AAPL"), hold=hold2)
    servicer._model = model2  # swap model so .calls is fresh
    got = await servicer.Predict(_make_fv("C"), None)
    assert got.mode == PredictionMode.PREDICTION_MODE_NORMAL


# --- Cancellation -----------------------------------------------------


async def test_predict_cancellation_propagates():
    # Client deadline expiry / call cancellation: server-side
    # coroutine receives CancelledError, must propagate (not catch
    # and synthesise a response — client isn't listening).
    hold = asyncio.Event()  # never set
    model = FakeModel(prediction=_normal("AAPL"), hold=hold)
    servicer = InferenceServicer(model)

    task = asyncio.create_task(servicer.Predict(_make_fv("AAPL"), None))
    await asyncio.sleep(0.01)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    # in_flight must decrement on cancel (finally block runs).
    assert servicer.in_flight == 0


# --- AsOf propagation -------------------------------------------------


async def test_predict_degraded_carries_request_as_of():
    model = FakeModel(exc=RuntimeError("down"))
    servicer = InferenceServicer(model)
    fv = _make_fv("AAPL")
    fv.as_of.CopyFrom(Timestamp(seconds=42))
    got = await servicer.Predict(fv, None)
    assert got.as_of.seconds == 42
