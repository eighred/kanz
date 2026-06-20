"""PRED-10 shadow / canary executor tests."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone

import pytest
from google.protobuf.timestamp_pb2 import Timestamp

from inference.v1.feature_vector_pb2 import FeatureValue, FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.registry import ModelMetadata, Registry, ValidationRecord
from kanz_inference.shadow import (
    LoggingShadowObserver,
    ShadowExecutor,
    ShadowObserver,
)
from kanz_inference.shadow.executor import REASON_INFERENCE_UNAVAILABLE


# --- Fakes ------------------------------------------------------------


class StubModel:
    """Records every predict call; returns a configured prediction
    or raises a configured exception. ``hold`` (if set) blocks on
    an event — used to test ordering."""

    def __init__(
        self,
        model_id: str,
        value: float = 0.5,
        exc: Exception | None = None,
        hold: asyncio.Event | None = None,
    ):
        self.model_id = model_id
        self._value = value
        self._exc = exc
        self._hold = hold
        self.calls: list[FeatureVector] = []

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        self.calls.append(fv)
        if self._hold is not None:
            await self._hold.wait()
        if self._exc is not None:
            raise self._exc
        return PredictionEnvelope(
            subject_id=fv.subject_id,
            model=self.model_id,
            value=self._value,
            mode=PredictionMode.PREDICTION_MODE_NORMAL,
            as_of=fv.as_of,
        )


@dataclass
class RecordingObserver:
    """Captures every (primary, shadow) pair the executor delivers.
    Implements ShadowObserver via structural typing."""

    pairs: list[tuple[str, float, str, float]] = field(default_factory=list)
    raise_on_observe: Exception | None = None

    async def observe(
        self,
        feature_vector: FeatureVector,
        primary: ModelMetadata,
        primary_prediction: PredictionEnvelope,
        shadow: ModelMetadata,
        shadow_prediction: PredictionEnvelope,
    ) -> None:
        self.pairs.append(
            (
                primary.model_id,
                primary_prediction.value,
                shadow.model_id,
                shadow_prediction.value,
            )
        )
        if self.raise_on_observe is not None:
            raise self.raise_on_observe


# --- Helpers ----------------------------------------------------------


def _meta(model_id: str, feature_set_ref: str = "equity-momentum:7") -> ModelMetadata:
    return ModelMetadata(
        model_id=model_id,
        feature_set_ref=feature_set_ref,
        confidence_threshold=0.5,
        artifact_uri=f"s3://x/{model_id}",
    )


def _register_primary(r: Registry, meta: ModelMetadata, model) -> None:
    """Record a passing validation then register primary — required by the
    MLOPS-01a gate. Shadows (primary=False) stay exempt and register directly."""
    now = datetime.now(timezone.utc)
    r.record_validation(
        ValidationRecord(
            model_id=meta.model_id,
            validated_at=now,
            expires_at=now + timedelta(days=365),
        )
    )
    r.register(meta, model)


def _fv(subject: str = "AAPL", feature_set_ref: str = "equity-momentum:7") -> FeatureVector:
    fv = FeatureVector()
    fv.subject_id = subject
    fv.feature_set_ref = feature_set_ref
    fv.as_of.CopyFrom(Timestamp(seconds=1767225600))
    fv.values["x"].CopyFrom(FeatureValue(scalar=1.0))
    return fv


# --- Constructor ------------------------------------------------------


def test_constructor_rejects_nil_registry():
    with pytest.raises(ValueError):
        ShadowExecutor(registry=None)  # type: ignore[arg-type]


# --- Predict happy path ----------------------------------------------


async def test_predict_returns_primary_prediction():
    r = Registry()
    primary_meta = _meta("primary@1.0.0")
    primary_model = StubModel(primary_meta.model_id, value=0.73)
    _register_primary(r, primary_meta, primary_model)

    exe = ShadowExecutor(r)
    got = await exe.predict(_fv("AAPL"))
    assert got.value == pytest.approx(0.73)
    assert got.model == primary_meta.model_id


async def test_predict_no_primary_returns_degraded_unavailable():
    # Empty registry — no primary for the feature_set_ref.
    exe = ShadowExecutor(Registry())
    got = await exe.predict(_fv("AAPL"))
    assert got.mode == PredictionMode.PREDICTION_MODE_DEGRADED
    assert got.degraded_reason == REASON_INFERENCE_UNAVAILABLE
    assert got.subject_id == "AAPL"
    assert got.value == 0.0


# --- Shadow fan-out ---------------------------------------------------


async def test_shadows_called_with_same_feature_vector():
    r = Registry()
    primary = StubModel("primary@1.0.0", value=0.5)
    shadow_a = StubModel("shadow-a@0.1.0", value=0.6)
    shadow_b = StubModel("shadow-b@0.1.0", value=0.7)
    _register_primary(r, _meta("primary@1.0.0"), primary)
    r.register(_meta("shadow-a@0.1.0"), shadow_a, primary=False)
    r.register(_meta("shadow-b@0.1.0"), shadow_b, primary=False)

    observer = RecordingObserver()
    exe = ShadowExecutor(r, observer=observer)

    fv = _fv("AAPL")
    await exe.predict(fv)
    await exe.wait_for_shadows()  # let fire-and-forget tasks complete

    assert len(primary.calls) == 1
    assert len(shadow_a.calls) == 1
    assert len(shadow_b.calls) == 1
    # All shadows saw the same feature vector instance.
    assert shadow_a.calls[0] is fv
    assert shadow_b.calls[0] is fv


async def test_observer_records_primary_shadow_pairs():
    r = Registry()
    _register_primary(r, _meta("primary@1.0.0"), StubModel("primary@1.0.0", value=1.0))
    r.register(_meta("shadow-a@0.1.0"), StubModel("shadow-a@0.1.0", value=1.5), primary=False)
    r.register(_meta("shadow-b@0.1.0"), StubModel("shadow-b@0.1.0", value=0.5), primary=False)

    observer = RecordingObserver()
    exe = ShadowExecutor(r, observer=observer)

    await exe.predict(_fv())
    await exe.wait_for_shadows()

    assert len(observer.pairs) == 2
    # Each pair records (primary_id, primary_value, shadow_id, shadow_value).
    by_shadow = {p[2]: p for p in observer.pairs}
    assert by_shadow["shadow-a@0.1.0"] == ("primary@1.0.0", 1.0, "shadow-a@0.1.0", 1.5)
    assert by_shadow["shadow-b@0.1.0"] == ("primary@1.0.0", 1.0, "shadow-b@0.1.0", 0.5)


# --- Failure isolation ------------------------------------------------


async def test_shadow_exception_does_not_affect_primary_response():
    r = Registry()
    _register_primary(r, _meta("primary@1.0.0"), StubModel("primary@1.0.0", value=0.42))
    r.register(
        _meta("broken-shadow@0.1.0"),
        StubModel("broken-shadow@0.1.0", exc=RuntimeError("shadow boom")),
        primary=False,
    )

    observer = RecordingObserver()
    exe = ShadowExecutor(r, observer=observer)

    got = await exe.predict(_fv())
    await exe.wait_for_shadows()

    # Primary response unaffected.
    assert got.value == pytest.approx(0.42)
    assert got.model == "primary@1.0.0"
    # Observer was NOT called for the failed shadow (predict raised
    # before observe could run).
    assert observer.pairs == []


async def test_observer_exception_does_not_affect_primary_response():
    r = Registry()
    _register_primary(r, _meta("primary@1.0.0"), StubModel("primary@1.0.0", value=0.42))
    r.register(_meta("shadow@0.1.0"), StubModel("shadow@0.1.0", value=0.43), primary=False)

    observer = RecordingObserver(raise_on_observe=RuntimeError("observer boom"))
    exe = ShadowExecutor(r, observer=observer)

    got = await exe.predict(_fv())
    await exe.wait_for_shadows()  # observer raises; ShadowExecutor isolates

    assert got.value == pytest.approx(0.42)
    # The pair WAS recorded before the raise (observer appended then raised).
    assert len(observer.pairs) == 1


async def test_no_shadows_means_no_observer_calls():
    r = Registry()
    _register_primary(r, _meta("primary@1.0.0"), StubModel("primary@1.0.0", value=0.5))

    observer = RecordingObserver()
    exe = ShadowExecutor(r, observer=observer)

    await exe.predict(_fv())
    await exe.wait_for_shadows()

    assert observer.pairs == []


# --- Ordering ---------------------------------------------------------


async def test_shadows_fire_after_primary_returns():
    # The primary's response must NOT wait for shadows. Verify by
    # making shadow.predict block forever and confirming the primary
    # call returns immediately.
    r = Registry()
    _register_primary(r, _meta("primary@1.0.0"), StubModel("primary@1.0.0", value=0.5))
    shadow_hold = asyncio.Event()  # never set ⇒ shadow blocks forever
    r.register(
        _meta("slow-shadow@0.1.0"),
        StubModel("slow-shadow@0.1.0", value=0.6, hold=shadow_hold),
        primary=False,
    )

    exe = ShadowExecutor(r)
    # If shadows blocked primary, this would hang and the test would
    # time out (pytest-asyncio default 60s).
    got = await asyncio.wait_for(exe.predict(_fv()), timeout=1.0)
    assert got.value == pytest.approx(0.5)


# --- Routing across feature_sets -------------------------------------


async def test_routes_by_feature_set_ref():
    # Two feature_sets, two primaries. ShadowExecutor routes per-call
    # based on the inbound feature_set_ref.
    r = Registry()
    _register_primary(r, _meta("equity-model@1", "equity-momentum:7"), StubModel("equity-model@1", value=1.0))
    _register_primary(r, _meta("fixed-model@1", "fixed-income:3"), StubModel("fixed-model@1", value=2.0))

    exe = ShadowExecutor(r)
    eq = await exe.predict(_fv(feature_set_ref="equity-momentum:7"))
    fi = await exe.predict(_fv(feature_set_ref="fixed-income:3"))

    assert eq.value == pytest.approx(1.0)
    assert fi.value == pytest.approx(2.0)


# --- LoggingShadowObserver -------------------------------------------


async def test_logging_observer_does_not_raise():
    # Smoke test the default observer — just confirm it accepts the
    # contract without erroring on a normal pair.
    obs = LoggingShadowObserver()
    fv = _fv()
    meta_p = _meta("p@1")
    meta_s = _meta("s@1")
    p = PredictionEnvelope(subject_id="x", model="p@1", value=1.0)
    s = PredictionEnvelope(subject_id="x", model="s@1", value=1.1)
    # Should not raise.
    await obs.observe(fv, meta_p, p, meta_s, s)


def test_logging_observer_satisfies_protocol():
    # Structural-typing check.
    assert isinstance(LoggingShadowObserver(), ShadowObserver)
