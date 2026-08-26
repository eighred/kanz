"""Envelope validation — publish-side rules.

Enforces the invariants in ``kanz-schemas/README.md § Envelope Policy`` §7
plus the per-class rules from the same file's § Event Class Rules.
``Producer.publish`` runs this after stamping; consumers should run it on receive too — the
envelope is the constitution and a defensive check is cheap.
"""

from __future__ import annotations

from envelope.v1.envelope_pb2 import Envelope, QualityFlag
from envelope.v1.event_class_pb2 import EventClass


def validate(env: Envelope) -> None:
    """Raise :class:`ValueError` on any violation."""
    if env is None:
        raise ValueError("envelope is None")
    if not env.event_id:
        raise ValueError("event_id required")
    if not env.event_type:
        raise ValueError("event_type required")
    if env.schema_version == 0:
        raise ValueError("schema_version required (must be >= 1)")
    if env.envelope_version == 0:
        raise ValueError("envelope_version required")
    if env.event_class == EventClass.EVENT_CLASS_UNSPECIFIED:
        raise ValueError("event_class must not be UNSPECIFIED")
    if not env.domain:
        raise ValueError("domain required")
    if not env.HasField("event_time"):
        raise ValueError("event_time required")
    if not env.HasField("ingestion_time"):
        raise ValueError("ingestion_time required")
    if not env.HasField("publish_time"):
        raise ValueError("publish_time required")
    if not env.correlation_id:
        raise ValueError("correlation_id required")
    if not env.source:
        raise ValueError("source required")
    # MT-01a — REQUIRED, and it was not checked here.
    #
    # The Go validator (pkg/bus.Validate) hard-rejects a live event with no
    # tenant_id. This one did not, and the Python producer had no tenant concept at
    # all — so EVERY event Python published was silently unconsumable by every Go
    # service on the platform. It was not dropped, either: the Go consumer nacks it,
    # the broker redelivers it, and the two spin. The AI-M1 loop hit exactly this —
    # 38,377 redeliveries of one prediction, acked never, while both sides logged
    # nothing wrong.
    #
    # The two clients must agree on the wire or the wire is a lie. This is that
    # agreement, enforced where it cannot be forgotten.
    if not env.tenant_id:
        raise ValueError(
            "tenant_id required (MT-01a): an untenanted event is rejected by every Go "
            "consumer on the live path and will redeliver forever. Set ProducerConfig.tenant, "
            "or publish from a handler so the inbound tenant is inherited."
        )
    if not env.producer_version:
        raise ValueError("producer_version required")
    if not env.idempotency_key:
        raise ValueError("idempotency_key required")
    if not env.payload_schema_ref:
        raise ValueError("payload_schema_ref required")
    # FACT events: idempotency_key MUST equal event_id (event-class-rules §1).
    if (
        env.event_class == EventClass.EVENT_CLASS_FACT
        and env.idempotency_key != env.event_id
    ):
        raise ValueError("FACT events require idempotency_key == event_id")
    # QUALITY_FLAG_REPLAYED is set only by replay tooling (EVT-20). A live
    # publisher must reject it before the bus.
    if QualityFlag.QUALITY_FLAG_REPLAYED in env.quality_flags:
        raise ValueError("live publish must not set QUALITY_FLAG_REPLAYED")
    # producer_sequence is per-(source, partition_key); 0 means N/A. Empty
    # partition_key ⇒ sequence must be 0 (envelope.proto field comment).
    if not env.partition_key and env.producer_sequence != 0:
        raise ValueError("producer_sequence must be 0 when partition_key is empty")
