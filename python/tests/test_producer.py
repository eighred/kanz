from __future__ import annotations

from datetime import datetime, timezone

import pytest
from google.protobuf.timestamp_pb2 import Timestamp

from kanz_bus import (
    Event,
    EventClass,
    Message,
    Producer,
    ProducerConfig,
    propagation_context,
    unframe,
)


class CaptureClient:
    """Fake :class:`Client` for stamping tests — captures published messages."""

    def __init__(self) -> None:
        self.sent: list[Message] = []

    async def publish(self, msg: Message) -> None:
        self.sent.append(msg)

    async def subscribe(self, subject, group, handler) -> None:
        raise NotImplementedError

    async def close(self) -> None:
        pass


def _make_producer() -> tuple[Producer, CaptureClient]:
    client = CaptureClient()
    p = Producer(
        client,
        ProducerConfig(source="test-svc/inst-1", producer_version="test-1.0.0", tenant="acme"),
    )
    return p, client


def _fact_event() -> Event:
    et = datetime(2026, 5, 15, 12, 0, 0, tzinfo=timezone.utc)
    return Event(
        subject="market.equity.trade",
        event_type="market.equity.trade",
        event_class=EventClass.EVENT_CLASS_FACT,
        schema_version=1,
        domain="market",
        event_time=et,
        partition_key="AAPL",
        payload_schema_ref="market.v1.MarketDataEvent:1",
        payload=Timestamp(seconds=int(et.timestamp())),
    )


async def test_producer_stamps_fact_event():
    p, cc = _make_producer()
    await p.publish(_fact_event())
    assert len(cc.sent) == 1
    sent = cc.sent[0]
    assert sent.subject == "market.equity.trade"
    assert sent.key == b"AAPL"

    env, _ = unframe(sent.body)
    assert env.event_id
    assert env.correlation_id == env.event_id  # root event
    assert env.idempotency_key == env.event_id  # FACT
    assert env.envelope_version == 1
    assert env.source == "test-svc/inst-1"
    assert env.producer_version == "test-1.0.0"
    assert env.producer_sequence == 1
    assert env.HasField("publish_time")
    assert env.HasField("ingestion_time")


async def test_producer_propagates_causation():
    p, cc = _make_producer()
    e = _fact_event()
    e.correlation_id = "root-corr-id"
    e.causation_id = "parent-event-id"
    await p.publish(e)
    env, _ = unframe(cc.sent[0].body)
    assert env.correlation_id == "root-corr-id"
    assert env.causation_id == "parent-event-id"


async def test_producer_sequence_increments_per_partition_key():
    p, cc = _make_producer()
    for pk in ("AAPL", "AAPL", "MSFT", "AAPL"):
        e = _fact_event()
        e.partition_key = pk
        await p.publish(e)
    seqs = [unframe(m.body)[0].producer_sequence for m in cc.sent]
    assert seqs == [1, 2, 1, 3]


async def test_producer_sequence_zero_without_partition_key():
    p, cc = _make_producer()
    e = _fact_event()
    e.partition_key = ""
    await p.publish(e)
    env, _ = unframe(cc.sent[0].body)
    assert env.producer_sequence == 0


async def test_producer_command_requires_idempotency_key():
    p, _ = _make_producer()
    e = _fact_event()
    e.event_class = EventClass.EVENT_CLASS_COMMAND
    e.event_type = "risk.command.rebalance"
    e.idempotency_key = ""
    with pytest.raises(ValueError, match="idempotency_key"):
        await p.publish(e)


async def test_producer_command_preserves_idempotency_key():
    p, cc = _make_producer()
    e = _fact_event()
    e.event_class = EventClass.EVENT_CLASS_COMMAND
    e.idempotency_key = "caller-key-123"
    await p.publish(e)
    env, _ = unframe(cc.sent[0].body)
    assert env.idempotency_key == "caller-key-123"
    assert env.idempotency_key != env.event_id


async def test_producer_rejects_mismatched_idempotency_key_for_fact():
    p, _ = _make_producer()
    e = _fact_event()
    e.idempotency_key = "not-the-event-id"
    with pytest.raises(ValueError, match="idempotency_key"):
        await p.publish(e)


async def test_producer_rejects_none_event_time():
    p, _ = _make_producer()
    e = _fact_event()
    e.event_time = None  # type: ignore[assignment]
    with pytest.raises(ValueError, match="event_time"):
        await p.publish(e)


async def test_producer_rejects_none_payload():
    p, _ = _make_producer()
    e = _fact_event()
    e.payload = None  # type: ignore[assignment]
    with pytest.raises(ValueError, match="payload"):
        await p.publish(e)


def test_new_producer_requires_source_and_version():
    with pytest.raises(ValueError):
        Producer(
            CaptureClient(),
            ProducerConfig(source="", producer_version="", tenant="acme"),
        )
    with pytest.raises(ValueError):
        Producer(
            CaptureClient(),
            ProducerConfig(source="x", producer_version="", tenant="acme"),
        )
    with pytest.raises(ValueError):
        Producer(
            None,  # type: ignore[arg-type]
            ProducerConfig(source="x", producer_version="v", tenant="acme"),
        )


async def test_producer_stamps_broker_dedup_header():
    p, cc = _make_producer()
    await p.publish(_fact_event())
    sent = cc.sent[0]
    env, _ = unframe(sent.body)
    assert env.idempotency_key
    assert sent.headers and sent.headers["Nats-Msg-Id"] == env.idempotency_key


async def test_producer_inherits_lineage_from_contextvars():
    p, cc = _make_producer()
    with propagation_context(
        correlation_id="corr-from-ctx",
        causation_id="parent-evt",
        trace_context="00-ctx-trace",
    ):
        await p.publish(_fact_event())
    env, _ = unframe(cc.sent[0].body)
    assert env.correlation_id == "corr-from-ctx"
    assert env.causation_id == "parent-evt"
    assert env.trace_context == "00-ctx-trace"


async def test_producer_explicit_fields_beat_contextvars():
    p, cc = _make_producer()
    e = _fact_event()
    e.correlation_id = "explicit-corr"
    e.causation_id = "explicit-causation"
    e.trace_context = "explicit-trace"
    with propagation_context(
        correlation_id="corr-from-ctx",
        causation_id="ctx-causation",
        trace_context="ctx-trace",
    ):
        await p.publish(e)
    env, _ = unframe(cc.sent[0].body)
    assert env.correlation_id == "explicit-corr"
    assert env.causation_id == "explicit-causation"
    assert env.trace_context == "explicit-trace"


async def test_producer_root_event_correlation_defaults_to_event_id():
    p, cc = _make_producer()
    await p.publish(_fact_event())
    env, _ = unframe(cc.sent[0].body)
    assert env.correlation_id == env.event_id
    assert env.causation_id == ""
