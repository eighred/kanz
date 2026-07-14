from __future__ import annotations

from datetime import datetime, timezone

import pytest
from google.protobuf.timestamp_pb2 import Timestamp

from kanz_bus import Envelope, EventClass, QualityFlag, validate


def _valid_envelope() -> Envelope:
    et = datetime(2026, 5, 15, 12, 0, 0, tzinfo=timezone.utc)
    ts = Timestamp(seconds=int(et.timestamp()))
    env = Envelope()
    env.event_id = "evt-1"
    env.event_type = "market.equity.trade"
    env.schema_version = 1
    env.envelope_version = 1
    env.event_class = EventClass.EVENT_CLASS_FACT
    env.domain = "market"
    env.event_time.CopyFrom(ts)
    env.ingestion_time.CopyFrom(ts)
    env.publish_time.CopyFrom(ts)
    env.correlation_id = "evt-1"
    env.source = "svc/inst"
    env.tenant_id = "acme"  # MT-01a: a live event without one is rejected by every Go consumer
    env.producer_version = "1.0.0"
    env.idempotency_key = "evt-1"  # FACT: equals event_id
    env.payload_schema_ref = "market.v1.MarketDataEvent:1"
    env.partition_key = "AAPL"
    env.producer_sequence = 1
    return env


def test_validate_accepts_canonical_envelope():
    validate(_valid_envelope())  # no raise


def test_validate_rejects_none():
    with pytest.raises(ValueError):
        validate(None)  # type: ignore[arg-type]


@pytest.mark.parametrize(
    "field_name,mutate",
    [
        ("event_id", lambda e: setattr(e, "event_id", "")),
        ("event_type", lambda e: setattr(e, "event_type", "")),
        ("schema_version", lambda e: setattr(e, "schema_version", 0)),
        ("envelope_version", lambda e: setattr(e, "envelope_version", 0)),
        (
            "event_class",
            lambda e: setattr(
                e, "event_class", EventClass.EVENT_CLASS_UNSPECIFIED
            ),
        ),
        ("domain", lambda e: setattr(e, "domain", "")),
        ("event_time", lambda e: e.ClearField("event_time")),
        ("ingestion_time", lambda e: e.ClearField("ingestion_time")),
        ("publish_time", lambda e: e.ClearField("publish_time")),
        ("correlation_id", lambda e: setattr(e, "correlation_id", "")),
        ("source", lambda e: setattr(e, "source", "")),
        ("producer_version", lambda e: setattr(e, "producer_version", "")),
        ("idempotency_key", lambda e: setattr(e, "idempotency_key", "")),
        ("payload_schema_ref", lambda e: setattr(e, "payload_schema_ref", "")),
    ],
)
def test_validate_rejects_missing_field(field_name, mutate):
    env = _valid_envelope()
    mutate(env)
    with pytest.raises(ValueError, match=field_name):
        validate(env)


def test_validate_rejects_fact_with_mismatched_idempotency_key():
    env = _valid_envelope()
    env.idempotency_key = "different-key"
    with pytest.raises(ValueError, match="FACT"):
        validate(env)


def test_validate_rejects_replayed_flag():
    env = _valid_envelope()
    env.quality_flags.append(QualityFlag.QUALITY_FLAG_REPLAYED)
    with pytest.raises(ValueError, match="REPLAYED"):
        validate(env)


def test_validate_rejects_sequence_without_partition_key():
    env = _valid_envelope()
    env.partition_key = ""
    env.producer_sequence = 5
    with pytest.raises(ValueError, match="producer_sequence"):
        validate(env)


def test_validate_allows_command_with_caller_idempotency_key():
    env = _valid_envelope()
    env.event_class = EventClass.EVENT_CLASS_COMMAND
    env.idempotency_key = "caller-key"  # != event_id is fine for COMMAND
    validate(env)  # no raise
