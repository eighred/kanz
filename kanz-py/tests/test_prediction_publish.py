"""PRED-05 prediction publish tests."""

from __future__ import annotations

from datetime import datetime, timezone

import pytest
from google.protobuf.timestamp_pb2 import Timestamp

from envelope.v1.envelope_pb2 import Envelope, QualityFlag
from envelope.v1.event_class_pb2 import EventClass
from envelope.v1.event_frame_pb2 import EventFrame
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_bus import Message, Producer, ProducerConfig, unframe, validate
from kanz_inference.publish import (
    SCHEMA_REF_PREDICTION,
    SUBJECT_PREDICTION_SCORED,
    Publisher,
)


# --- Fakes ------------------------------------------------------------


class CaptureClient:
    """Bus client that records every Publish for inspection. Mirror
    of the captureClient used in the Go-side publish tests
    (kanz/internal/risk/publish/publish_test.go).
    """

    def __init__(self) -> None:
        self.sent: list[Message] = []

    async def publish(self, message: Message) -> None:
        self.sent.append(message)

    async def subscribe(self, subject, group, handler):
        raise NotImplementedError("not used in publish tests")

    async def close(self) -> None:
        pass


def _make_publisher() -> tuple[Publisher, CaptureClient]:
    client = CaptureClient()
    producer = Producer(
        client,
        ProducerConfig(source="inference-svc/test", producer_version="inf-1.0.0", tenant="acme"),
    )
    return Publisher(producer), client


def _make_envelope(event_id: str = "evt-feat-1") -> Envelope:
    env = Envelope()
    env.event_id = event_id
    env.correlation_id = "root-corr-1"
    env.trace_context = "00-trace-1"
    return env


def _make_prediction(
    subject_id: str = "AAPL",
    mode: int = PredictionMode.PREDICTION_MODE_NORMAL,
) -> PredictionEnvelope:
    return PredictionEnvelope(
        subject_id=subject_id,
        model="vol-forecast@1.4.2",
        value=0.73,
        confidence=0.9,
        mode=mode,
        as_of=Timestamp(seconds=1767225600),
    )


# --- Tests ------------------------------------------------------------


def test_constructor_rejects_nil_producer():
    with pytest.raises(ValueError):
        Publisher(None)  # type: ignore[arg-type]


async def test_publish_normal_emits_valid_envelope_no_quality_flags():
    pub, client = _make_publisher()
    src = _make_envelope()
    pred = _make_prediction(mode=PredictionMode.PREDICTION_MODE_NORMAL)

    await pub.publish_prediction(src, pred)

    assert len(client.sent) == 1
    msg = client.sent[0]
    assert msg.subject == SUBJECT_PREDICTION_SCORED
    assert msg.key == b"AAPL"
    env, _payload = unframe(msg.body)
    validate(env)  # raises on contract violation
    assert env.event_class == EventClass.EVENT_CLASS_FACT
    assert env.domain == "inference"
    assert env.payload_schema_ref == SCHEMA_REF_PREDICTION
    # NORMAL ⇒ no QUALITY_FLAG_DEGRADED on envelope.
    assert QualityFlag.QUALITY_FLAG_DEGRADED not in list(env.quality_flags)


async def test_publish_degraded_sets_quality_flag_degraded():
    # PRED-02 §3 producer obligation — degraded predictions get the
    # envelope flag so payload-blind observability sees the signal.
    pub, client = _make_publisher()
    src = _make_envelope()
    pred = _make_prediction(mode=PredictionMode.PREDICTION_MODE_DEGRADED)
    pred.degraded_reason = "low_confidence"

    await pub.publish_prediction(src, pred)

    env, _ = unframe(client.sent[0].body)
    assert QualityFlag.QUALITY_FLAG_DEGRADED in list(env.quality_flags)


async def test_publish_lineage_propagated_to_envelope():
    # The originating feature event's correlation_id flows into the
    # prediction envelope's correlation_id; the feature event's
    # event_id becomes the prediction's causation_id (chain link).
    pub, client = _make_publisher()
    src = _make_envelope("evt-feature-99")
    src.correlation_id = "corr-shared"
    src.trace_context = "00-some-trace"

    await pub.publish_prediction(src, _make_prediction())

    env, _ = unframe(client.sent[0].body)
    assert env.correlation_id == "corr-shared"
    assert env.causation_id == "evt-feature-99"
    assert env.trace_context == "00-some-trace"


async def test_publish_partition_key_is_subject_id():
    # Per-subject ordering must hold end-to-end: features for
    # AAPL → inference → predictions for AAPL. partition_key
    # locks this in at every hop.
    pub, client = _make_publisher()
    await pub.publish_prediction(
        _make_envelope(), _make_prediction(subject_id="MSFT")
    )
    msg = client.sent[0]
    assert msg.key == b"MSFT"
    env, _ = unframe(msg.body)
    assert env.partition_key == "MSFT"


async def test_publish_payload_round_trip():
    pub, client = _make_publisher()
    pred = _make_prediction()
    pred.explanation["return_5d"] = 0.4
    pred.feature_vector_event_id = "evt-feat-source"

    await pub.publish_prediction(_make_envelope(), pred)

    _, payload_bytes = unframe(client.sent[0].body)
    got = PredictionEnvelope()
    got.ParseFromString(payload_bytes)
    assert got.subject_id == "AAPL"
    assert got.model == "vol-forecast@1.4.2"
    assert got.value == pytest.approx(0.73)
    assert got.confidence == pytest.approx(0.9)
    assert got.explanation["return_5d"] == pytest.approx(0.4)
    assert got.feature_vector_event_id == "evt-feat-source"


async def test_publish_nil_envelope_rejected():
    pub, _ = _make_publisher()
    with pytest.raises(ValueError):
        await pub.publish_prediction(None, _make_prediction())  # type: ignore[arg-type]


async def test_publish_nil_prediction_rejected():
    pub, _ = _make_publisher()
    with pytest.raises(ValueError):
        await pub.publish_prediction(_make_envelope(), None)  # type: ignore[arg-type]


async def test_publish_missing_subject_id_rejected():
    pub, _ = _make_publisher()
    pred = _make_prediction()
    pred.subject_id = ""
    with pytest.raises(ValueError):
        await pub.publish_prediction(_make_envelope(), pred)


async def test_publish_missing_as_of_rejected():
    pub, _ = _make_publisher()
    pred = _make_prediction()
    pred.ClearField("as_of")
    with pytest.raises(ValueError):
        await pub.publish_prediction(_make_envelope(), pred)


async def test_publish_emits_fact_with_idempotency_equals_event_id():
    # Sanity check that the FACT contract (idempotency_key == event_id)
    # holds end-to-end through the Publisher → Producer → wire path.
    pub, client = _make_publisher()
    await pub.publish_prediction(_make_envelope(), _make_prediction())
    env, _ = unframe(client.sent[0].body)
    assert env.idempotency_key == env.event_id, "FACT idempotency invariant"


async def test_publish_satisfies_streaming_worker_protocol():
    # Structural-typing check: the streaming worker type-hints its
    # publisher as `PredictionPublisher` (the Protocol). Verify our
    # concrete Publisher class is duck-compatible — has the right
    # method shape.
    from kanz_inference.streaming.worker import PredictionPublisher as Proto

    pub, _ = _make_publisher()
    assert isinstance(pub, Proto)  # runtime_checkable check would need decorator;
    # fall back to method-presence check:
    assert hasattr(pub, "publish_prediction")
    assert callable(pub.publish_prediction)
