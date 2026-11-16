"""Bus publish of model-registry mutations (DEBT-02b, #112).

Implements the ``RegistryPublisher`` protocol declared in
``kanz_inference.registry.coordination`` — the seam ``CoordinatedRegistry``
calls after applying a mutation locally.

# What was missing, and why it mattered

``RegistryPublisher`` was a ``Protocol`` with no concrete implementation, so a
``CoordinatedRegistry`` could only be constructed around a test double. The
registry that "converges across replicas" converged in tests and nowhere else,
and the Go side's ``Log`` was in the same state — an interface with an in-memory
default and no binding.

Each side also had its own event type and neither could read the other's:
``RegistryEvent.to_dict`` produced JSON; Go's ``registry.Event`` had no wire form
at all. ``inference.v1.ModelRegistryEvent`` is the shared contract, and this
module is the producer that puts it on the bus.

# Subject and ordering

Published on ``platform.model.registered`` — the long-retention stream, because
this log is the SOURCE OF TRUTH a starting replica replays from offset 0 to
rebuild its registry, not a stream of transient scores.

**Keyed by model_id.** A model's ``record_validation`` and its ``primary``
register must stay in per-partition order, or a replica applies the promotion
before the validation and the MLOPS-01a gate refuses it — a divergence that
appears on some replicas and not others depending on partition timing, which is
the hardest kind to reproduce.

# It publishes failures too

A validation that did NOT pass is published like any other. It is evidence that
the model was graded and rejected, and dropping it would make "failed its
validation" and "was never validated" the same observable state on every replica
that rebuilt from the log.
"""

from __future__ import annotations

from datetime import datetime, timezone

from envelope.v1.event_class_pb2 import EventClass
from google.protobuf.timestamp_pb2 import Timestamp
from inference.v1.model_registry_pb2 import (
    ModelRegistryEvent,
    ModelRegistryOp,
    ModelRole,
)

from kanz_bus import Event, Producer
from kanz_inference.registry.coordination import (
    OP_RECORD_VALIDATION,
    OP_REGISTER,
    OP_UNREGISTER,
    RegistryEvent,
)

# The subject carries the registry log. platform.> is provisioned at 168h — see
# the module docstring for why this belongs there rather than under inference.>.
SUBJECT_MODEL_REGISTERED = "platform.model.registered"
SCHEMA_REF_MODEL_REGISTRY = "inference.v1.ModelRegistryEvent:1"
SCHEMA_VERSION_MODEL_REGISTRY = 1
DOMAIN_PLATFORM = "platform"

# The wire enum for each local op name. A mapping rather than a naming
# convention, so an op added on one side without the other is a KeyError at the
# publish site instead of an UNSPECIFIED that a consumer silently ignores.
_OPS = {
    OP_REGISTER: ModelRegistryOp.MODEL_REGISTRY_OP_REGISTER,
    OP_UNREGISTER: ModelRegistryOp.MODEL_REGISTRY_OP_UNREGISTER,
    OP_RECORD_VALIDATION: ModelRegistryOp.MODEL_REGISTRY_OP_RECORD_VALIDATION,
}


class BusRegistryPublisher:
    """Bus-backed implementation of the ``RegistryPublisher`` protocol.

    Construct once per process around a configured ``kanz_bus.Producer`` and
    hand it to ``CoordinatedRegistry``. Named ``BusRegistryPublisher`` so it does
    not collide with the ``RegistryPublisher`` Protocol it satisfies — Python's
    structural typing means no explicit declaration is needed.
    """

    def __init__(self, producer: Producer) -> None:
        if producer is None:
            raise ValueError("registry/publish: producer is required")
        self._producer = producer

    async def publish(self, event: RegistryEvent) -> None:
        """Publish one registry mutation as a ``platform.model.registered`` FACT.

        Raises ValueError on a mutation that cannot be represented on the wire.
        A registry event that is dropped instead is worse than one that fails
        here: every replica rebuilding from the log would be missing it, and
        nothing would say so.
        """
        if event is None:
            raise ValueError("registry/publish: event is required")
        if not event.model_id:
            # model_id is the partition key. Without it the ordering guarantee
            # the MLOPS-01a gate depends on does not exist for this event, and
            # it would be distributed to an arbitrary partition.
            raise ValueError("registry/publish: model_id is required (it is the partition key)")
        if not event.origin:
            # origin is what stops a replica applying its own event a second
            # time. An empty one makes every replica treat it as someone else's.
            raise ValueError("registry/publish: origin is required (loop prevention)")
        try:
            op = _OPS[event.op]
        except KeyError:
            raise ValueError(f"registry/publish: unknown op {event.op!r}") from None

        payload = ModelRegistryEvent(
            op=op,
            origin=event.origin,
            model_id=event.model_id,
            feature_set_ref=event.feature_set_ref,
            confidence_threshold=event.confidence_threshold,
            artifact_uri=event.artifact_uri,
            # primary is a bool locally and a role on the wire, because the
            # registry has exactly two roles and a bool cannot carry a third.
            role=(
                ModelRole.MODEL_ROLE_PRIMARY
                if event.primary
                else ModelRole.MODEL_ROLE_CANDIDATE
            ),
            passed=event.passed,
            report_uri=event.report_uri,
        )
        if event.validated_at is not None:
            payload.validated_at.CopyFrom(_ts(event.validated_at))
        if event.expires_at is not None:
            payload.expires_at.CopyFrom(_ts(event.expires_at))

        await self._producer.publish(
            Event(
                subject=SUBJECT_MODEL_REGISTERED,
                event_type=SUBJECT_MODEL_REGISTERED,
                event_class=EventClass.EVENT_CLASS_FACT,
                schema_version=SCHEMA_VERSION_MODEL_REGISTRY,
                domain=DOMAIN_PLATFORM,
                # WHEN THE MUTATION HAPPENED. For a validation that is when it
                # ran; otherwise now. A registry event with no event_time is
                # rejected by the envelope validator, and stamping the publish
                # time onto a validation would misdate the evidence.
                event_time=event.validated_at or datetime.now(timezone.utc),
                partition_key=event.model_id,
                payload_schema_ref=SCHEMA_REF_MODEL_REGISTRY,
                payload=payload,
            )
        )


def _ts(dt: datetime) -> Timestamp:
    """UTC-normalise before converting.

    A naive datetime is interpreted by FromDatetime as local time, so a
    validation stamped on a pod in one timezone would land on the wire at a
    different instant from an identical one elsewhere — and the expiry the gate
    reads would move with it.
    """
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    ts = Timestamp()
    ts.FromDatetime(dt.astimezone(timezone.utc))
    return ts
