"""MLOPS-01c drift → revalidation loop tests."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from envelope.v1.envelope_pb2 import Envelope
from observation.v1.data_quality_pb2 import DataQualityEvent, Severity

from kanz_inference.governance import (
    SUBJECT_DRIFT_DETECTED,
    DriftTrigger,
    LoggingRevalidationSink,
    RevalidationRequest,
    StaticModelResolver,
)


# --- Fakes ------------------------------------------------------------


class RecordingSink:
    def __init__(self) -> None:
        self.requests: list[RevalidationRequest] = []

    async def request_revalidation(self, request: RevalidationRequest) -> None:
        self.requests.append(request)


class FakeSubscriber:
    async def subscribe(self, subject, group, handler) -> None:
        self.subject = subject
        self.group = group
        self.handler = handler


def _drift_payload(
    feature: str = "rsi_14",
    metric: str = "psi",
    score: float = 0.31,
    threshold: float = 0.2,
    severity: int = Severity.SEVERITY_WARNING,
    subject: str = "inference.feature.computed",
    partition_key: str = "AAPL",
    baseline_ref: str = "baseline-2026Q1",
) -> bytes:
    event = DataQualityEvent(subject=subject, partition_key=partition_key, severity=severity, summary="drift")
    event.drift.feature = feature
    event.drift.metric = metric
    event.drift.score = score
    event.drift.threshold = threshold
    event.drift.baseline_ref = baseline_ref
    return event.SerializeToString()


def _gap_payload() -> bytes:
    event = DataQualityEvent(subject="market.equity.trade", severity=Severity.SEVERITY_CRITICAL, summary="gap")
    event.gap.expected_sequence = 5
    event.gap.received_sequence = 9
    return event.SerializeToString()


# --- Resolution + emit ------------------------------------------------


async def test_drift_emits_request_with_evidence():
    sink = RecordingSink()
    resolver = StaticModelResolver({"rsi_14": ["vol-forecast@1.4.2"]})
    trigger = DriftTrigger(resolver, sink)

    await trigger.handle(Envelope(), _drift_payload())

    assert len(sink.requests) == 1
    req = sink.requests[0]
    assert req.model_id == "vol-forecast@1.4.2"
    assert req.feature == "rsi_14"
    assert req.metric == "psi"
    assert req.score == pytest.approx(0.31)
    assert req.threshold == pytest.approx(0.2)
    assert req.baseline_ref == "baseline-2026Q1"
    assert req.partition_key == "AAPL"
    assert "rsi_14" in req.reason and "psi" in req.reason


async def test_drift_fans_out_to_every_affected_model():
    sink = RecordingSink()
    resolver = StaticModelResolver({"rsi_14": ["a@1", "b@2", "c@3"]})
    trigger = DriftTrigger(resolver, sink)

    await trigger.handle(Envelope(), _drift_payload(feature="rsi_14"))

    assert {r.model_id for r in sink.requests} == {"a@1", "b@2", "c@3"}


async def test_drift_on_unused_feature_emits_nothing():
    sink = RecordingSink()
    resolver = StaticModelResolver({"other": ["a@1"]})
    trigger = DriftTrigger(resolver, sink)

    await trigger.handle(Envelope(), _drift_payload(feature="rsi_14"))

    assert sink.requests == []


# --- Variant + severity gating ----------------------------------------


async def test_non_drift_variant_is_ignored():
    sink = RecordingSink()
    trigger = DriftTrigger(StaticModelResolver({"x": ["a@1"]}), sink)

    await trigger.handle(Envelope(), _gap_payload())

    assert sink.requests == []


async def test_below_min_severity_is_ignored():
    sink = RecordingSink()
    trigger = DriftTrigger(
        StaticModelResolver({"rsi_14": ["a@1"]}),
        sink,
        min_severity=Severity.SEVERITY_CRITICAL,
    )

    # WARNING < CRITICAL ⇒ skipped.
    await trigger.handle(Envelope(), _drift_payload(severity=Severity.SEVERITY_WARNING))
    assert sink.requests == []

    # CRITICAL ⇒ fires.
    await trigger.handle(Envelope(), _drift_payload(severity=Severity.SEVERITY_CRITICAL))
    assert len(sink.requests) == 1


async def test_empty_feature_is_skipped():
    sink = RecordingSink()
    trigger = DriftTrigger(StaticModelResolver({"": ["a@1"]}), sink)

    await trigger.handle(Envelope(), _drift_payload(feature=""))

    assert sink.requests == []


# --- Cooldown ---------------------------------------------------------


async def test_cooldown_suppresses_repeat_then_fires_after_window():
    clock = {"now": datetime(2026, 6, 21, 12, 0, 0, tzinfo=timezone.utc)}
    sink = RecordingSink()
    trigger = DriftTrigger(
        StaticModelResolver({"rsi_14": ["a@1"]}),
        sink,
        cooldown=timedelta(hours=1),
        clock=lambda: clock["now"],
    )

    await trigger.handle(Envelope(), _drift_payload())
    await trigger.handle(Envelope(), _drift_payload())  # within cooldown
    assert len(sink.requests) == 1  # second suppressed

    clock["now"] = clock["now"] + timedelta(hours=2)  # past cooldown
    await trigger.handle(Envelope(), _drift_payload())
    assert len(sink.requests) == 2


async def test_cooldown_is_per_model_feature():
    clock = {"now": datetime(2026, 6, 21, 12, 0, 0, tzinfo=timezone.utc)}
    sink = RecordingSink()
    trigger = DriftTrigger(
        StaticModelResolver({"rsi_14": ["a@1"], "macd": ["a@1"]}),
        sink,
        cooldown=timedelta(hours=1),
        clock=lambda: clock["now"],
    )

    # Same model, two different features — distinct cooldown keys, both fire.
    await trigger.handle(Envelope(), _drift_payload(feature="rsi_14"))
    await trigger.handle(Envelope(), _drift_payload(feature="macd"))
    assert len(sink.requests) == 2


# --- Plumbing ---------------------------------------------------------


async def test_run_subscribes_to_drift_subject():
    sub = FakeSubscriber()
    trigger = DriftTrigger(StaticModelResolver({}), RecordingSink())
    await trigger.run(sub, "mlops-revalidation")
    assert sub.subject == SUBJECT_DRIFT_DETECTED
    assert sub.group == "mlops-revalidation"
    assert sub.handler == trigger.handle


async def test_malformed_payload_raises():
    trigger = DriftTrigger(StaticModelResolver({}), RecordingSink())
    with pytest.raises(Exception):
        await trigger.handle(Envelope(), b"\xff\xff not a proto")


def test_constructor_rejects_nil_collaborators():
    with pytest.raises(ValueError):
        DriftTrigger(None, RecordingSink())  # type: ignore[arg-type]
    with pytest.raises(ValueError):
        DriftTrigger(StaticModelResolver({}), None)  # type: ignore[arg-type]


async def test_logging_sink_smoke():
    trigger = DriftTrigger(StaticModelResolver({"rsi_14": ["a@1"]}), LoggingRevalidationSink())
    # Should not raise.
    await trigger.handle(Envelope(), _drift_payload())
