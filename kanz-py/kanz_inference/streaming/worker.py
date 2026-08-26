"""Streaming inference worker — drains ``inference.feature.computed``
events from the bus, runs them through a configured ``Model``, and
publishes the resulting ``PredictionEnvelope`` via a
``PredictionPublisher``.

The worker is the producer side of PRED-02's degraded contract: it
catches the model failure modes (exception, timeout in §2.2 — to be
added when the timeout primitive lands) and emits a DEGRADED
PredictionEnvelope rather than dropping the request. PRED-08's
interactive worker pool reuses the same degraded-trigger logic so
both paths agree on what counts as degraded.
"""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass, field
from typing import Awaitable, Callable, Optional, Protocol, runtime_checkable

from envelope.v1.envelope_pb2 import Envelope
from inference.v1.feature_vector_pb2 import FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_bus import EventHandler, Subscriber
from kanz_inference.observability import metrics

logger = logging.getLogger(__name__)


# Subject the worker subscribes to. Matches PRED-03's Go publisher
# constant + kanz-schemas/README.md § Subject Taxonomy §1 example.
SUBJECT_FEATURE_COMPUTED = "inference.feature.computed"


class Model(Protocol):
    """A scoring model the worker drives. ``predict`` is async so
    models can call out to GPU servers / remote inference backends
    without blocking the consumer event loop.

    Implementations MUST set ``mode`` on the returned envelope
    honestly per the PRED-02 contract: ``NORMAL`` for full-trust
    output, ``DEGRADED`` for low-confidence / fallback paths. The
    worker layers an additional DEGRADED on top only when predict
    raises (PRED-02 §2.1 — inference unavailable).
    """

    model_id: str  # "{name}@{version}", e.g. "vol-forecast@1.4.2"

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        ...


@runtime_checkable
class PredictionPublisher(Protocol):
    """Boundary PRED-05 implements. The worker hands one prediction
    + the originating envelope to publish; the publisher constructs
    a new bus envelope (preserving correlation_id for lineage) and
    publishes ``inference.prediction.scored``.

    ``@runtime_checkable`` so callers can ``isinstance(x, PredictionPublisher)``
    — useful for the cross-module compliance test that PRED-05's
    concrete Publisher satisfies the protocol.
    """

    async def publish_prediction(
        self, source_envelope: Envelope, prediction: PredictionEnvelope
    ) -> None:
        ...


@dataclass
class LastKnownCache:
    """In-memory cache of the most recent NORMAL prediction per
    subject. Read by the degraded-fallback path when ``Model.predict``
    raises — implements PRED-02 §2.1's "cache fallback" behaviour.

    No TTL (cached PredictionEnvelope carries its own ``as_of``,
    consumers can branch on staleness). Eviction is overwrite-on-
    next-store. Single asyncio event loop ⇒ no lock needed; dict
    read/write is GIL-atomic. Mirrors the RISK-11 ``Cache`` shape.
    """

    _entries: dict[str, PredictionEnvelope] = field(default_factory=dict)

    def store(self, subject_id: str, prediction: PredictionEnvelope) -> None:
        if not subject_id:
            return
        self._entries[subject_id] = prediction

    def lookup(self, subject_id: str) -> Optional[PredictionEnvelope]:
        return self._entries.get(subject_id)


# Degraded-reason identifiers from kanz-schemas/README.md
# § Inference Degraded-Mode Contract §2. Constants so downstream alert-matching is exact.
REASON_INFERENCE_UNAVAILABLE = "inference_unavailable"
REASON_NO_CACHED_PREDICTION = "no_cached_prediction"


class StreamingWorker:
    """Wires a ``Subscriber`` (bus consumer) to a ``Model`` and a
    ``PredictionPublisher``. Construct once per (worker process,
    model); call ``run`` to start the subscription loop.

    The worker is the canonical example of an EventHandler that
    decodes a payload, performs domain work, and re-publishes —
    same shape as the risk module's ``ingest.Ingestor`` on the Go
    side. ``handle`` is exposed (not underscore-prefixed) so tests
    can drive it directly without a fake Subscriber.
    """

    def __init__(
        self,
        subscriber: Subscriber,
        model: Model,
        publisher: PredictionPublisher,
        cache: Optional[LastKnownCache] = None,
    ):
        if subscriber is None:
            raise ValueError("subscriber required")
        if model is None:
            raise ValueError("model required")
        if publisher is None:
            raise ValueError("publisher required")
        self._subscriber = subscriber
        self._model = model
        self._publisher = publisher
        self._cache = cache if cache is not None else LastKnownCache()

    async def run(self, group: str) -> None:
        """Subscribes to ``inference.feature.computed`` under the
        given consumer group. Blocks until the subscriber returns
        (typically cancellation). One worker = one group; horizontal
        scale = additional worker processes sharing the group.
        """
        await self._subscriber.subscribe(SUBJECT_FEATURE_COMPUTED, group, self.handle)

    async def handle(self, env: Envelope, payload: bytes) -> None:
        """One inbound feature event. Public so tests can drive it
        without spinning up the full subscriber loop.
        """
        fv = FeatureVector()
        try:
            fv.ParseFromString(payload)
        except Exception as e:  # noqa: BLE001 — re-raised after logging
            # An undecodable payload never reaches a model, so it appears in NO
            # prediction series at all. Counted here or it is invisible outside
            # the log, which is where a schema skew between the Go publisher and
            # this consumer would otherwise sit unnoticed.
            metrics.STREAM_EVENTS.labels(
                outcome=metrics.STREAM_OUTCOME_UNMARSHAL_FAILED
            ).inc()
            logger.error("feature payload unmarshal failed: %s", e)
            raise

        started = time.perf_counter()
        try:
            prediction = await self._model.predict(fv)
        except Exception as e:  # noqa: BLE001 — degraded-fallback path
            logger.warning(
                "model.predict raised for %s: %s — falling back", fv.subject_id, e
            )
            prediction = self._fallback(env, fv)
        finally:
            metrics.PREDICT_SECONDS.labels(path=metrics.PATH_STREAMING).observe(
                time.perf_counter() - started
            )
        metrics.observe_prediction(metrics.PATH_STREAMING, prediction)

        # Only NORMAL predictions seed the cache. Caching a DEGRADED
        # prediction would let it serve later requests as if it were
        # fresh — exactly the trust-laundering PRED-02 §1 forbids.
        if prediction.mode == PredictionMode.PREDICTION_MODE_NORMAL:
            self._cache.store(fv.subject_id, prediction)

        try:
            await self._publisher.publish_prediction(env, prediction)
        except Exception:  # noqa: BLE001 — counted, then re-raised unchanged
            # A prediction that was scored and never published is the one outcome
            # the prediction counters CANNOT show: they already counted it. The
            # exception still propagates so the subscriber naks and the event is
            # redelivered — this adds a series, not a behaviour.
            metrics.STREAM_EVENTS.labels(
                outcome=metrics.STREAM_OUTCOME_PUBLISH_FAILED
            ).inc()
            raise
        # END TO END: scored AND on the bus. This is the rate a stalled-consumer
        # alert watches, because it is the only one that goes to zero when the
        # durable subscription wedges.
        metrics.STREAM_EVENTS.labels(outcome=metrics.STREAM_OUTCOME_HANDLED).inc()

    def _fallback(
        self, env: Envelope, fv: FeatureVector
    ) -> PredictionEnvelope:
        """Build a degraded prediction per PRED-02 §2.1: cache hit ⇒
        re-emit cached value with DEGRADED mode + ``inference_unavailable``
        reason; cache miss ⇒ zero-valued degraded with ``no_cached_prediction``.
        """
        cached = self._cache.lookup(fv.subject_id)
        if cached is not None:
            return PredictionEnvelope(
                subject_id=fv.subject_id,
                model=cached.model,
                value=cached.value,
                confidence=cached.confidence,
                class_label=cached.class_label,
                explanation=dict(cached.explanation),
                mode=PredictionMode.PREDICTION_MODE_DEGRADED,
                degraded_reason=REASON_INFERENCE_UNAVAILABLE,
                feature_vector_event_id=env.event_id,
                as_of=fv.as_of,
            )
        return PredictionEnvelope(
            subject_id=fv.subject_id,
            model=self._model.model_id,
            value=0.0,
            confidence=0.0,
            mode=PredictionMode.PREDICTION_MODE_DEGRADED,
            degraded_reason=REASON_NO_CACHED_PREDICTION,
            feature_vector_event_id=env.event_id,
            as_of=fv.as_of,
        )
