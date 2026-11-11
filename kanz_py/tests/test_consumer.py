from __future__ import annotations

from datetime import datetime, timezone

import pytest
from google.protobuf.timestamp_pb2 import Timestamp

from kanz_bus import (
    Consumer,
    ConsumerConfig,
    DedupWindow,
    Envelope,
    Event,
    EventClass,
    EventFrame,
    Handler,
    Message,
    Producer,
    ProducerConfig,
    RetryConfig,
    get_causation_id,
    get_correlation_id,
    get_trace_context,
    unframe,
)


class OneShotSub:
    """Fires Subscribe's handler exactly once with the canned message."""

    def __init__(self, msg: Message) -> None:
        self.msg = msg

    async def subscribe(self, subject: str, group: str, handler: Handler) -> None:
        await handler(self.msg)


class CaptureClient:
    def __init__(self) -> None:
        self.sent: list[Message] = []

    async def publish(self, msg: Message) -> None:
        self.sent.append(msg)

    async def subscribe(self, *args) -> None:
        raise NotImplementedError

    async def close(self) -> None:
        pass


def _envelope(
    event_id: str = "evt-inbound",
    correlation_id: str = "evt-inbound",
    trace: str = "",
    tenant_id: str = "acme",
) -> Envelope:
    et = datetime(2026, 5, 15, 12, 0, 0, tzinfo=timezone.utc)
    ts = Timestamp(seconds=int(et.timestamp()))
    env = Envelope()
    env.event_id = event_id
    env.event_type = "x.y.z"
    env.schema_version = 1
    env.envelope_version = 1
    env.event_class = EventClass.EVENT_CLASS_FACT
    env.domain = "market"
    env.event_time.CopyFrom(ts)
    env.ingestion_time.CopyFrom(ts)
    env.publish_time.CopyFrom(ts)
    env.correlation_id = correlation_id
    env.tenant_id = tenant_id  # MT-01a
    env.source = "svc/inst"
    env.producer_version = "1.0"
    env.idempotency_key = event_id
    env.payload_schema_ref = "x.y.z:1"
    env.trace_context = trace
    return env


def _frame(env: Envelope, payload: bytes = b"") -> bytes:
    return EventFrame(envelope=env, payload=payload).SerializeToString()


async def test_consumer_stashes_propagation_on_contextvars():
    env = _envelope(event_id="evt-A", correlation_id="corr-1", trace="00-trace-01")
    sub = OneShotSub(Message(subject="x.y.z", body=_frame(env, b"p")))
    c = Consumer(sub)

    captured = {}

    async def handler(received_env: Envelope, payload: bytes) -> None:
        captured["corr"] = get_correlation_id()
        captured["caus"] = get_causation_id()
        captured["trace"] = get_trace_context()
        captured["event_id"] = received_env.event_id
        captured["payload"] = payload

    await c.subscribe("x", "g", handler)

    assert captured["event_id"] == "evt-A"
    assert captured["payload"] == b"p"
    assert captured["corr"] == "corr-1"
    assert captured["caus"] == "evt-A"  # causation = inbound event_id
    assert captured["trace"] == "00-trace-01"


async def test_consumer_empty_trace_not_stashed():
    env = _envelope(trace="")  # hot-path sample skip
    sub = OneShotSub(Message(subject="x.y.z", body=_frame(env)))
    c = Consumer(sub)

    captured_trace = ""

    async def handler(env: Envelope, payload: bytes) -> None:
        nonlocal captured_trace
        captured_trace = get_trace_context()

    await c.subscribe("x", "g", handler)
    assert captured_trace == ""


async def test_consumer_resets_contextvars_after_handler():
    env = _envelope(event_id="evt-A", correlation_id="corr-1", trace="00-trace-01")
    sub = OneShotSub(Message(subject="x.y.z", body=_frame(env)))
    c = Consumer(sub)

    async def handler(env: Envelope, payload: bytes) -> None:
        pass

    await c.subscribe("x", "g", handler)
    # After subscribe returns, contextvars are back to defaults.
    assert get_correlation_id() == ""
    assert get_causation_id() == ""
    assert get_trace_context() == ""


async def test_consumer_unframe_failure_surfaces():
    sub = OneShotSub(Message(subject="x.y.z", body=b"garbage"))
    c = Consumer(sub)

    handler_called = False

    async def handler(env: Envelope, payload: bytes) -> None:
        nonlocal handler_called
        handler_called = True

    with pytest.raises(ValueError):
        await c.subscribe("x", "g", handler)
    assert not handler_called


async def test_consumer_validation_failure_surfaces():
    env = _envelope()
    env.event_id = ""  # invalidates the envelope
    sub = OneShotSub(Message(subject="x.y.z", body=_frame(env)))
    c = Consumer(sub)

    handler_called = False

    async def handler(env: Envelope, payload: bytes) -> None:
        nonlocal handler_called
        handler_called = True

    with pytest.raises(ValueError):
        await c.subscribe("x", "g", handler)
    assert not handler_called


async def test_consumer_to_producer_chains_lineage():
    inbound = _envelope(
        event_id="evt-A", correlation_id="corr-root", trace="00-trace-01"
    )
    sub = OneShotSub(Message(subject="x.y.z", body=_frame(inbound, b"a-payload")))
    c = Consumer(sub)

    cc = CaptureClient()
    p = Producer(cc, ProducerConfig(source="test/inst", producer_version="v1", tenant="acme"))

    et = datetime(2026, 5, 15, 12, 0, 0, tzinfo=timezone.utc)

    async def handler(env: Envelope, payload: bytes) -> None:
        await p.publish(
            Event(
                subject="y",
                event_type="downstream.event",
                event_class=EventClass.EVENT_CLASS_FACT,
                schema_version=1,
                domain="market",
                event_time=et,
                partition_key="k",
                payload_schema_ref="downstream.v1:1",
                payload=Timestamp(seconds=int(et.timestamp())),
            )
        )

    await c.subscribe("x", "g", handler)

    assert len(cc.sent) == 1
    out, _ = unframe(cc.sent[0].body)
    assert out.correlation_id == "corr-root"  # inherited
    assert out.causation_id == "evt-A"  # = inbound event_id
    assert out.trace_context == "00-trace-01"  # inherited


def test_new_consumer_rejects_none():
    with pytest.raises(ValueError):
        Consumer(None)  # type: ignore[arg-type]


class MultiShotSub:
    """Simulates broker redelivery: invokes handler `times` times.
    Errors are recorded but do not short-circuit."""

    def __init__(self, msg: Message, times: int) -> None:
        self.msg = msg
        self.times = times

    async def subscribe(self, subject: str, group: str, handler: Handler) -> None:
        last_exc: BaseException | None = None
        for _ in range(self.times):
            try:
                await handler(self.msg)
            except Exception as e:
                last_exc = e
        if last_exc is not None:
            raise last_exc


class FailingPublisher:
    async def publish(self, msg: Message) -> None:
        raise RuntimeError("dlq down")


def _fast_retry(attempts: int) -> RetryConfig:
    return RetryConfig(
        max_attempts=attempts,
        initial_backoff_seconds=0.001,
        max_backoff_seconds=0.002,
    )


async def test_consumer_retries_until_success():
    env = _envelope(event_id="evt-A")
    sub = OneShotSub(Message(subject="x.y.z", body=_frame(env)))
    c = Consumer(sub, ConsumerConfig(retry=_fast_retry(3)))

    calls = 0

    async def handler(env: Envelope, payload: bytes) -> None:
        nonlocal calls
        calls += 1
        if calls < 3:
            raise RuntimeError("transient")

    await c.subscribe("x", "g", handler)
    assert calls == 3


async def test_consumer_surfaces_after_retry_without_dlq():
    sub = OneShotSub(Message(subject="x.y.z", body=_frame(_envelope())))
    c = Consumer(sub, ConsumerConfig(retry=_fast_retry(3)))

    calls = 0

    async def handler(env: Envelope, payload: bytes) -> None:
        nonlocal calls
        calls += 1
        raise RuntimeError("always-fail")

    with pytest.raises(RuntimeError, match="always-fail"):
        await c.subscribe("x", "g", handler)
    assert calls == 3


async def test_consumer_routes_to_dlq_after_retries():
    env = _envelope(event_id="evt-A")
    sub = OneShotSub(Message(subject="x.y.z", body=_frame(env)))
    dlq = CaptureClient()
    c = Consumer(sub, ConsumerConfig(retry=_fast_retry(3), dlq=dlq))

    calls = 0

    async def handler(env: Envelope, payload: bytes) -> None:
        nonlocal calls
        calls += 1
        raise RuntimeError("poison")

    await c.subscribe("market.equity.trade", "g", handler)  # no raise
    assert calls == 3
    assert len(dlq.sent) == 1
    sent = dlq.sent[0]
    assert sent.subject == "dlq.market.equity.trade"
    assert sent.headers is not None
    assert sent.headers["Kanz-DLQ-Original-Subject"] == "market.equity.trade"
    assert sent.headers["Kanz-DLQ-Attempts"] == "3"
    assert "poison" in sent.headers["Kanz-DLQ-Error"]


async def test_consumer_routes_unframe_failure_to_dlq():
    sub = OneShotSub(Message(subject="x.y.z", body=b"garbage"))
    dlq = CaptureClient()
    c = Consumer(sub, ConsumerConfig(dlq=dlq))

    async def handler(env: Envelope, payload: bytes) -> None:
        pytest.fail("handler should not run")

    await c.subscribe("x.y.z", "g", handler)
    assert len(dlq.sent) == 1
    assert dlq.sent[0].subject == "dlq.x.y.z"
    assert dlq.sent[0].headers["Kanz-DLQ-Attempts"] == "0"


async def test_consumer_routes_validation_failure_to_dlq():
    env = _envelope()
    env.event_id = ""  # invalidates
    sub = OneShotSub(Message(subject="x.y.z", body=_frame(env)))
    dlq = CaptureClient()
    c = Consumer(sub, ConsumerConfig(dlq=dlq))

    async def handler(env: Envelope, payload: bytes) -> None:
        pytest.fail("handler should not run")

    await c.subscribe("x", "g", handler)
    assert len(dlq.sent) == 1


async def test_consumer_dedups_repeated_delivery():
    env = _envelope(event_id="evt-A")
    sub = MultiShotSub(Message(subject="x.y.z", body=_frame(env)), times=3)
    c = Consumer(sub)  # default config: dedup on

    calls = 0

    async def handler(env: Envelope, payload: bytes) -> None:
        nonlocal calls
        calls += 1

    await c.subscribe("x", "g", handler)
    assert calls == 1


async def test_consumer_no_record_on_handler_failure():
    env = _envelope(event_id="evt-A")
    sub = MultiShotSub(Message(subject="x.y.z", body=_frame(env)), times=3)
    c = Consumer(sub)  # default: 1 attempt, no DLQ

    calls = 0

    async def handler(env: Envelope, payload: bytes) -> None:
        nonlocal calls
        calls += 1
        raise RuntimeError("transient")

    with pytest.raises(RuntimeError):
        await c.subscribe("x", "g", handler)
    assert calls == 3  # all delivered; no record because no success


async def test_consumer_dedup_disabled():
    env = _envelope(event_id="evt-A")
    sub = MultiShotSub(Message(subject="x.y.z", body=_frame(env)), times=3)
    c = Consumer(sub, ConsumerConfig(dedup_window=DedupWindow(ttl_seconds=0)))

    calls = 0

    async def handler(env: Envelope, payload: bytes) -> None:
        nonlocal calls
        calls += 1

    await c.subscribe("x", "g", handler)
    assert calls == 3  # all delivered; dedup disabled


async def test_consumer_dlq_records_dedup():
    """After DLQ-routing terminal failure, subsequent duplicates dedup."""
    env = _envelope(event_id="evt-poison")
    sub = MultiShotSub(Message(subject="x.y.z", body=_frame(env)), times=3)
    dlq = CaptureClient()
    c = Consumer(sub, ConsumerConfig(retry=_fast_retry(1), dlq=dlq))

    calls = 0

    async def handler(env: Envelope, payload: bytes) -> None:
        nonlocal calls
        calls += 1
        raise RuntimeError("poison")

    await c.subscribe("x", "g", handler)
    assert calls == 1  # first delivery dispatched; rest dedup'd
    assert len(dlq.sent) == 1


async def test_consumer_dlq_publish_failure_surfaces():
    sub = OneShotSub(Message(subject="x.y.z", body=_frame(_envelope())))
    c = Consumer(sub, ConsumerConfig(retry=_fast_retry(1), dlq=FailingPublisher()))

    async def handler(env: Envelope, payload: bytes) -> None:
        raise RuntimeError("poison")

    with pytest.raises(RuntimeError, match="dlq publish failed"):
        await c.subscribe("x", "g", handler)
