"""Shadow / canary executor (PRED-10).

Wraps PRED-09's Registry to satisfy the streaming + interactive
workers' Model protocol while transparently fanning out to shadow
models for evaluation. The served response is ALWAYS the primary's
output; shadow predictions are observed asynchronously without
affecting the primary path's latency or correctness.

# Why shadows run AFTER the primary, not in parallel

The primary's response is what consumers see. If shadows ran in
parallel with the primary and we used asyncio.gather, the response
latency would be bounded by the slowest task — defeating the
canary's "risk-free evaluation" premise. By awaiting the primary
first and firing shadows in the background (asyncio.create_task,
no await), the caller's latency budget tracks the primary only.

The trade-off: a shadow that depends on something the primary's
execution side-effected (cache warmup, GPU memory pre-allocation)
might see different behavior than it would in parallel. Acceptable
because (a) shadows are evaluation-only, not consumer-facing;
(b) any such coupling indicates a stateful Model implementation,
which is itself a correctness concern PRED-04/08's per-call model
contract doesn't permit.

# Shadow failure isolation

Each shadow's call is wrapped in a try/except in
_run_shadow_and_observe. Exceptions are logged but never re-
raised. The primary's response has already been returned to the
caller by the time a shadow can fail — there's no path by which a
shadow exception affects what the consumer sees.

# Task tracking

Shadow tasks are added to ``_shadow_tasks`` (a set) and removed via
``add_done_callback``. Without this, Python's garbage collector
could reclaim the task before it completes, silently dropping
shadow work. The set also enables ``wait_for_shadows()`` for
graceful shutdown and deterministic test completion.
"""

from __future__ import annotations

import asyncio
import logging
from typing import Optional, Protocol, runtime_checkable

from google.protobuf.timestamp_pb2 import Timestamp

from inference.v1.feature_vector_pb2 import FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.registry import ModelMetadata, Registry

logger = logging.getLogger(__name__)


# Degraded-reason identifier reused from PRED-04 / PRED-08 — when
# no primary is registered for the inbound feature_set_ref, the
# inference layer is effectively unavailable for that route.
REASON_INFERENCE_UNAVAILABLE = "inference_unavailable"


@runtime_checkable
class ShadowObserver(Protocol):
    """Records one (primary, shadow) prediction pair for offline
    comparison. Implementations might log, write to a metrics
    store, or emit an observation.v1.DecisionLog event.

    ``observe`` is async to allow remote sinks (database write,
    metrics push) without blocking the shadow path. Exceptions
    raised here propagate to the shadow task's error handler and
    are logged — same isolation as a shadow predict failure.
    """

    async def observe(
        self,
        feature_vector: FeatureVector,
        primary: ModelMetadata,
        primary_prediction: PredictionEnvelope,
        shadow: ModelMetadata,
        shadow_prediction: PredictionEnvelope,
    ) -> None:
        ...


class LoggingShadowObserver:
    """Default observer: logs each (primary, shadow) pair at INFO.
    Acceptable for low-volume canary; switch to a metrics-store
    observer for high-volume production where a log line per
    prediction would overwhelm log aggregation.
    """

    async def observe(
        self,
        feature_vector: FeatureVector,
        primary: ModelMetadata,
        primary_prediction: PredictionEnvelope,
        shadow: ModelMetadata,
        shadow_prediction: PredictionEnvelope,
    ) -> None:
        logger.info(
            "shadow comparison subject=%s primary=%s primary_value=%.6f "
            "shadow=%s shadow_value=%.6f delta=%.6f",
            feature_vector.subject_id,
            primary.model_id,
            primary_prediction.value,
            shadow.model_id,
            shadow_prediction.value,
            shadow_prediction.value - primary_prediction.value,
        )


class ShadowExecutor:
    """Satisfies the Model protocol; routes requests through
    Registry primary + shadows. Construct once per worker process
    and hand to StreamingWorker / InferenceServicer in place of a
    bare Model.

    Routing happens at predict time on ``fv.feature_set_ref`` — a
    single ShadowExecutor handles every feature_set the worker
    receives, so the worker is not bound to one feature_set at
    construction time.
    """

    # The Model protocol requires a model_id attribute. ShadowExecutor
    # routes across many models so there is no single canonical id;
    # the composite name is the honest answer. Per-response
    # PredictionEnvelope.model carries the actual scoring model's id
    # for traceability.
    model_id = "shadow-executor"

    def __init__(self, registry: Registry, observer: Optional[ShadowObserver] = None):
        if registry is None:
            raise ValueError("registry required")
        self._registry = registry
        self._observer = observer
        self._shadow_tasks: set[asyncio.Task] = set()

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        primary = self._registry.primary_for_feature_set(fv.feature_set_ref)
        if primary is None:
            return self._no_primary_response(fv)

        primary_meta, primary_model = primary
        primary_pred = await primary_model.predict(fv)

        # Fire shadows in the background — they execute AFTER the
        # primary returns its response, so primary latency is not
        # affected by shadow throughput.
        shadows = self._registry.shadows_for_feature_set(fv.feature_set_ref)
        for shadow_meta, shadow_model in shadows:
            task = asyncio.create_task(
                self._run_shadow_and_observe(
                    fv, primary_meta, primary_pred, shadow_meta, shadow_model
                )
            )
            self._shadow_tasks.add(task)
            task.add_done_callback(self._shadow_tasks.discard)

        return primary_pred

    async def wait_for_shadows(self) -> None:
        """Wait for all in-flight shadow tasks to complete. Useful
        for graceful shutdown (drain before exit) and for tests
        that need deterministic completion of fire-and-forget
        shadow work. Exceptions are gathered, not raised — shadow
        failures are observed via the per-task handler in
        ``_run_shadow_and_observe`` and should not surface here.
        """
        if not self._shadow_tasks:
            return
        # Snapshot before await — _shadow_tasks mutates as tasks
        # complete and the done-callback runs.
        in_flight = list(self._shadow_tasks)
        await asyncio.gather(*in_flight, return_exceptions=True)

    async def _run_shadow_and_observe(
        self,
        fv: FeatureVector,
        primary_meta: ModelMetadata,
        primary_pred: PredictionEnvelope,
        shadow_meta: ModelMetadata,
        shadow_model,
    ) -> None:
        try:
            shadow_pred = await shadow_model.predict(fv)
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001 — shadow failures are isolated
            logger.warning(
                "shadow predict failed: model=%s subject=%s error=%s",
                shadow_meta.model_id,
                fv.subject_id,
                e,
            )
            return

        if self._observer is None:
            return
        try:
            await self._observer.observe(
                fv, primary_meta, primary_pred, shadow_meta, shadow_pred
            )
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001
            logger.warning(
                "shadow observe failed: shadow=%s subject=%s error=%s",
                shadow_meta.model_id,
                fv.subject_id,
                e,
            )

    def _no_primary_response(self, fv: FeatureVector) -> PredictionEnvelope:
        """No primary registered for the inbound feature_set_ref —
        the inference layer cannot serve this route. Returns a
        DEGRADED envelope per PRED-02 §2.1 with reason
        ``inference_unavailable``.
        """
        as_of = fv.as_of if fv.HasField("as_of") else Timestamp()
        return PredictionEnvelope(
            subject_id=fv.subject_id,
            value=0.0,
            confidence=0.0,
            mode=PredictionMode.PREDICTION_MODE_DEGRADED,
            degraded_reason=REASON_INFERENCE_UNAVAILABLE,
            as_of=as_of,
        )
