"""Bus publish of prediction events (PRED-05).

Implements the ``PredictionPublisher`` protocol declared in
``kanz_inference.streaming.worker`` — the boundary the streaming
worker (PRED-04) calls after running ``Model.predict``. Wire-
symmetric counterpart to the Go-side risk/publish (RISK-10):
wraps a shared ``kanz_bus.Producer``, translates the
``PredictionEnvelope`` payload into a framed bus message, propagates
lineage via contextvars, and sets ``QUALITY_FLAG_DEGRADED`` on the
envelope when the prediction's ``mode`` is DEGRADED — per PRED-02
§3 producer obligation.

Class name is ``Publisher`` (module-scoped: ``kanz_inference.publish.Publisher``)
rather than ``PredictionPublisher`` so it does NOT collide with the
``PredictionPublisher`` Protocol the streaming worker uses for type-
hinting its boundary. Python structural typing means this class
still satisfies the Protocol without an explicit declaration —
duck-typing at the method-signature level.
"""

from __future__ import annotations

from envelope.v1.envelope_pb2 import Envelope, QualityFlag
from envelope.v1.event_class_pb2 import EventClass
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_bus import Event, Producer, propagation_context

# Subject + schema-ref constants for the prediction FACT. Subject
# matches kanz-schemas/docs/subject-taxonomy.md §1 example
# ("inference.prediction.scored"); schema-ref points at the
# registered PredictionEnvelope payload (PRED-01).
SUBJECT_PREDICTION_SCORED = "inference.prediction.scored"
SCHEMA_REF_PREDICTION = "inference.v1.PredictionEnvelope:1"
SCHEMA_VERSION_PREDICTION = 1


class Publisher:
    """Bus-backed implementation of the streaming worker's
    PredictionPublisher protocol. Construct once per worker process
    around a configured ``kanz_bus.Producer`` and hand to the
    StreamingWorker.
    """

    def __init__(self, producer: Producer) -> None:
        if producer is None:
            raise ValueError("publish: producer is required")
        self._producer = producer

    async def publish_prediction(
        self, source_envelope: Envelope, prediction: PredictionEnvelope
    ) -> None:
        """Publish one prediction as an ``inference.prediction.scored``
        FACT. ``source_envelope`` is the envelope of the originating
        FeatureVector event — its correlation_id + event_id thread
        the lineage chain so the prediction's envelope inherits the
        correlation and points its causation_id at the feature event.

        Raises ValueError on contract violations the kanz_bus
        producer can't catch alone (missing subject_id, zero as_of).
        """
        if source_envelope is None:
            raise ValueError("publish: source_envelope is required")
        if prediction is None:
            raise ValueError("publish: prediction is required")
        if not prediction.subject_id:
            raise ValueError("publish: prediction.subject_id required")
        if not prediction.HasField("as_of"):
            raise ValueError("publish: prediction.as_of required")

        # PRED-02 §3 producer obligation: when mode == DEGRADED the
        # envelope must carry QUALITY_FLAG_DEGRADED so payload-blind
        # observability (data-integrity layer DATA-*) sees the signal
        # without parsing the payload. NORMAL ⇒ no flags.
        quality_flags: list[int] = []
        if prediction.mode == PredictionMode.PREDICTION_MODE_DEGRADED:
            quality_flags = [QualityFlag.QUALITY_FLAG_DEGRADED]

        # Lineage via contextvars (EVT-18c): propagation_context
        # stashes correlation/causation/trace, the Producer reads
        # them in _stamp. Causation is the SOURCE event's event_id
        # (the prediction was caused by that feature event), NOT
        # its causation_id — same chain-linking rule as EVT-17c.
        with propagation_context(
            correlation_id=source_envelope.correlation_id,
            causation_id=source_envelope.event_id,
            trace_context=source_envelope.trace_context,
        ):
            await self._producer.publish(
                Event(
                    subject=SUBJECT_PREDICTION_SCORED,
                    event_type=SUBJECT_PREDICTION_SCORED,
                    event_class=EventClass.EVENT_CLASS_FACT,
                    schema_version=SCHEMA_VERSION_PREDICTION,
                    domain="inference",
                    event_time=prediction.as_of.ToDatetime(),
                    partition_key=prediction.subject_id,
                    payload_schema_ref=SCHEMA_REF_PREDICTION,
                    payload=prediction,
                    quality_flags=quality_flags,
                )
            )
