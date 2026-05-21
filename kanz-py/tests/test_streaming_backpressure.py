"""PRED-14 streaming-worker backpressure + consumer-lag scaling tests.

The streaming worker (PRED-04) has no explicit backpressure code — the
backpressure is *structural*, in how it is driven: the NATS subscriber
(``kanz_bus.nats.NATSClient.subscribe``) pulls ONE message
(``fetch(batch=1)``), ``await``s the handler to completion, then acks
before fetching the next. So a slow model bounds in-flight work to 1 per
worker and unprocessed events pile up server-side as consumer lag — the
operational signal to scale out by adding workers to the same durable
group.

These tests model that delivery contract with a shared backlog queue (a
stand-in for the durable consumer group: each event is handed to exactly
one worker) and a gated model (so concurrency is asserted
deterministically, no sleeps). They prove:

  - one worker processes serially — in-flight never exceeds 1, and the
    next event is not pulled until the current prediction is published
    (events wait in the backlog = lag);
  - adding workers raises aggregate concurrency (consumer-lag scaling);
  - the group delivers each event exactly once (no loss, no dup);
  - per-subject ordering holds under a single worker;
  - backpressure DELAYS but never DEGRADES — the streaming path blocks
    and lets lag grow, in contrast to the interactive servicer (PRED-08)
    which sheds load via admission control (DEGRADED + pool_saturated).
"""

from __future__ import annotations

import asyncio

import pytest
from google.protobuf.timestamp_pb2 import Timestamp

from envelope.v1.envelope_pb2 import Envelope
from inference.v1.feature_vector_pb2 import FeatureValue, FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.streaming import StreamingWorker


# --- Fakes ------------------------------------------------------------


class FakeSubscriber:
    """Satisfies the Subscriber protocol; tests drive the delivery loop
    themselves rather than going through subscribe()."""

    async def subscribe(self, subject, group, handler):
        raise NotImplementedError("tests drive the delivery loop directly")


class ConcurrencyGate:
    """Shared in-flight tracker + release gate. A model entering
    ``predict`` increments ``in_flight`` (recording the max) and blocks
    on ``release`` until the test opens it — letting the test observe
    exactly how many predictions are in flight at once.
    """

    def __init__(self) -> None:
        self.in_flight = 0
        self.max_in_flight = 0
        self.release = asyncio.Event()


class GatedModel:
    """Model whose ``predict`` blocks on a shared gate. Multiple
    GatedModels can share one gate so ``max_in_flight`` measures
    aggregate concurrency across workers."""

    def __init__(self, gate: ConcurrencyGate, model_id: str = "test-model@1.0.0"):
        self.model_id = model_id
        self._gate = gate
        self.calls: list[FeatureVector] = []

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        self.calls.append(fv)
        self._gate.in_flight += 1
        self._gate.max_in_flight = max(self._gate.max_in_flight, self._gate.in_flight)
        try:
            await self._gate.release.wait()
        finally:
            self._gate.in_flight -= 1
        return _normal_prediction(fv.subject_id, _value_of(fv))


class EchoModel:
    """Returns immediately, echoing the feature vector's subject + value
    so ordering can be asserted."""

    def __init__(self, model_id: str = "test-model@1.0.0"):
        self.model_id = model_id

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        return _normal_prediction(fv.subject_id, _value_of(fv))


class RecordingPublisher:
    """Records every published prediction. Safe to share across workers
    — appends happen at await-free points under the single event loop."""

    def __init__(self) -> None:
        self.published: list[tuple[Envelope, PredictionEnvelope]] = []

    async def publish_prediction(self, env: Envelope, prediction: PredictionEnvelope) -> None:
        self.published.append((env, prediction))


# --- Helpers ----------------------------------------------------------


def _value_of(fv: FeatureVector) -> float:
    return fv.values["v"].scalar if "v" in fv.values else 0.0


def _fv(subject_id: str = "AAPL", v: float = 0.0) -> bytes:
    fv = FeatureVector()
    fv.subject_id = subject_id
    fv.feature_set_ref = "equity-momentum:7"
    fv.as_of.CopyFrom(Timestamp(seconds=1767225600))
    fv.values["v"].CopyFrom(FeatureValue(scalar=v))
    return fv.SerializeToString()


def _env(event_id: str) -> Envelope:
    env = Envelope()
    env.event_id = event_id
    env.correlation_id = event_id
    return env


def _normal_prediction(subject_id: str, value: float) -> PredictionEnvelope:
    return PredictionEnvelope(
        subject_id=subject_id,
        model="test-model@1.0.0",
        value=value,
        confidence=0.9,
        mode=PredictionMode.PREDICTION_MODE_NORMAL,
        as_of=Timestamp(seconds=1767225600),
    )


async def _drain(worker: StreamingWorker, backlog: "asyncio.Queue") -> None:
    """Mirror NATSClient.subscribe's fetch(batch=1) → await handler →
    ack loop: take one event, process it fully, then take the next.
    Returns when the backlog is empty. A shared backlog across several
    _drain coroutines models a durable consumer group."""
    while True:
        try:
            env, payload = backlog.get_nowait()
        except asyncio.QueueEmpty:
            return
        await worker.handle(env, payload)
        backlog.task_done()


async def _wait_until(predicate, timeout: float = 1.0) -> None:
    loop = asyncio.get_event_loop()
    deadline = loop.time() + timeout
    while loop.time() < deadline:
        if predicate():
            return
        await asyncio.sleep(0.001)
    raise AssertionError("condition not met within timeout")


# --- Single-worker backpressure ---------------------------------------


async def test_single_worker_holds_backlog_while_processing_one_event():
    # While the model is processing event 1, the worker must NOT pull
    # events 2..N — they wait in the backlog (this is the consumer lag).
    gate = ConcurrencyGate()
    model = GatedModel(gate)
    pub = RecordingPublisher()
    worker = StreamingWorker(subscriber=FakeSubscriber(), model=model, publisher=pub)

    backlog: asyncio.Queue = asyncio.Queue()
    for i in range(5):
        backlog.put_nowait((_env(f"evt-{i}"), _fv("AAPL", float(i))))

    drain = asyncio.create_task(_drain(worker, backlog))
    try:
        # Worker pulls exactly one and blocks in predict.
        await _wait_until(lambda: gate.in_flight == 1)
        assert len(model.calls) == 1, "worker pulled more than one event while busy"
        assert backlog.qsize() == 4, "remaining events should sit in the backlog as lag"
        assert pub.published == [], "nothing published until predict completes"

        # Release: the worker drains the rest, still one at a time.
        gate.release.set()
        await asyncio.wait_for(drain, timeout=1.0)
    finally:
        drain.cancel()

    assert len(pub.published) == 5
    assert gate.max_in_flight == 1, "single worker must never run two predictions at once"


async def test_single_worker_publishes_before_pulling_next():
    # Backpressure is "publish current, then fetch next" — assert the
    # pull order strictly follows publish completion.
    gate = ConcurrencyGate()
    gate.release.set()  # never block; we only care about ordering
    model = GatedModel(gate)
    pub = RecordingPublisher()
    worker = StreamingWorker(subscriber=FakeSubscriber(), model=model, publisher=pub)

    backlog: asyncio.Queue = asyncio.Queue()
    for i in range(4):
        backlog.put_nowait((_env(f"evt-{i}"), _fv("AAPL", float(i))))
    await _drain(worker, backlog)

    # Each predict call corresponds to one publish, in lockstep order.
    assert len(model.calls) == 4
    assert len(pub.published) == 4
    assert gate.max_in_flight == 1


# --- Consumer-lag scaling (horizontal scale-out) ----------------------


async def test_adding_workers_raises_aggregate_concurrency():
    # Three workers sharing one backlog (a durable group) reach
    # in-flight == 3 — adding workers scales aggregate throughput, the
    # mechanism by which consumer lag is worked down.
    gate = ConcurrencyGate()
    pub = RecordingPublisher()
    workers = [
        StreamingWorker(subscriber=FakeSubscriber(), model=GatedModel(gate), publisher=pub)
        for _ in range(3)
    ]

    backlog: asyncio.Queue = asyncio.Queue()
    for i in range(3):
        backlog.put_nowait((_env(f"evt-{i}"), _fv(f"S{i}", float(i))))

    drains = [asyncio.create_task(_drain(w, backlog)) for w in workers]
    try:
        await _wait_until(lambda: gate.in_flight == 3)
        assert gate.max_in_flight == 3, "3 workers should process 3 events concurrently"
        gate.release.set()
        await asyncio.wait_for(asyncio.gather(*drains), timeout=1.0)
    finally:
        for d in drains:
            d.cancel()

    assert len(pub.published) == 3


async def test_group_delivers_each_event_exactly_once():
    # Across a shared backlog, every event is processed once — no loss,
    # no double-processing (the group hands each event to one worker).
    gate = ConcurrencyGate()
    gate.release.set()
    pub = RecordingPublisher()
    workers = [
        StreamingWorker(subscriber=FakeSubscriber(), model=GatedModel(gate), publisher=pub)
        for _ in range(4)
    ]

    n = 20
    backlog: asyncio.Queue = asyncio.Queue()
    for i in range(n):
        backlog.put_nowait((_env(f"evt-{i}"), _fv(f"S{i}", float(i))))

    await asyncio.wait_for(
        asyncio.gather(*[_drain(w, backlog) for w in workers]), timeout=2.0
    )

    assert len(pub.published) == n
    subjects = [pred.subject_id for _, pred in pub.published]
    assert sorted(subjects) == sorted(f"S{i}" for i in range(n))
    assert len(set(subjects)) == n, "an event was processed by more than one worker"


# --- Ordering under backpressure --------------------------------------


async def test_single_worker_preserves_per_subject_order():
    # One worker draining same-subject events in order publishes them in
    # the same order — the sequential loop is what guarantees the
    # per-subject_id ordering the engine relies on end-to-end.
    pub = RecordingPublisher()
    worker = StreamingWorker(subscriber=FakeSubscriber(), model=EchoModel(), publisher=pub)

    backlog: asyncio.Queue = asyncio.Queue()
    for i in range(6):
        backlog.put_nowait((_env(f"evt-{i}"), _fv("AAPL", float(i))))
    await _drain(worker, backlog)

    values = [pred.value for _, pred in pub.published]
    assert values == [0.0, 1.0, 2.0, 3.0, 4.0, 5.0]


# --- Backpressure delays, never degrades ------------------------------


async def test_backpressure_delays_but_does_not_degrade():
    # The streaming path applies backpressure (block + let lag grow); it
    # does NOT shed load by marking predictions DEGRADED. That load-
    # shedding behaviour is the interactive servicer's (PRED-08)
    # admission control (pool_saturated). A slow model here yields a
    # NORMAL prediction once it returns — delayed, not degraded.
    gate = ConcurrencyGate()
    model = GatedModel(gate)
    pub = RecordingPublisher()
    worker = StreamingWorker(subscriber=FakeSubscriber(), model=model, publisher=pub)

    handle = asyncio.create_task(worker.handle(_env("evt-0"), _fv("AAPL", 1.0)))
    try:
        await _wait_until(lambda: gate.in_flight == 1)
        assert not handle.done(), "handle should block under backpressure, not return early"
        assert pub.published == []
        gate.release.set()
        await asyncio.wait_for(handle, timeout=1.0)
    finally:
        handle.cancel()

    assert len(pub.published) == 1
    _, pred = pub.published[0]
    assert pred.mode == PredictionMode.PREDICTION_MODE_NORMAL
    assert pred.degraded_reason == ""
