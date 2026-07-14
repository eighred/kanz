"""Envelope stamping + framed publish (EVT-18b).

Mirrors the Go ``kanz/pkg/bus`` Producer (EVT-17b) so events produced from
Python and Go are wire-identical.
"""

from __future__ import annotations

import os
import time
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone

from envelope.v1.envelope_pb2 import Envelope
from envelope.v1.event_class_pb2 import EventClass
from envelope.v1.event_frame_pb2 import EventFrame
from google.protobuf.message import Message as ProtoMessage

from kanz_bus.bus import Client, Message
from kanz_bus.propagation import (
    get_tenant_id,
    get_causation_id,
    get_correlation_id,
    get_trace_context,
)
from kanz_bus.validate import validate

# Envelope schema version this client builds against. Bumps once per
# additive change to envelope.proto (rare; envelope-policy §6).
ENVELOPE_VERSION = 1


def uuid7() -> uuid.UUID:
    """Generate an RFC 9562 v7 UUID.

    Python's stdlib ``uuid`` gained ``uuid7`` in 3.13 — this client supports
    3.10+, so we build the value: 48-bit Unix-ms timestamp + 4-bit version +
    12-bit random + 2-bit variant + 62-bit random.
    """
    ms = int(time.time() * 1000) & 0xFFFFFFFFFFFF
    rand_a = int.from_bytes(os.urandom(2), "big") & 0x0FFF
    rand_b = int.from_bytes(os.urandom(8), "big") & 0x3FFFFFFFFFFFFFFF
    val = (ms << 80) | (0x7 << 76) | (rand_a << 64) | (0b10 << 62) | rand_b
    return uuid.UUID(int=val)


@dataclass
class ProducerConfig:
    """Identity fields the producer stamps on every event."""

    source: str
    """``service/instance`` — e.g. ``"market-ingest/pod-7"``."""

    producer_version: str
    """git SHA or semver of the producing code."""

    tenant: str = ""
    """MT-01a fallback tenant, stamped on events that carry neither an explicit
    ``Event.tenant_id`` nor an inbound tenant on the context.

    A SINGLE-TENANT service sets it. A multi-tenant one leaves it empty and lets
    each event carry its own — either explicitly, or inherited from the event that
    caused it (the Consumer stashes the inbound tenant, so a derived event belongs
    to the tenant of the event it was derived from). Exactly the Go client's rule."""


@dataclass
class Event:
    """Producer-facing form of a Kanz event: caller-known envelope fields +
    the domain payload. Auto fields (event_id, publish_time, source,
    producer_version, producer_sequence, envelope_version, correlation_id
    for roots, idempotency_key for non-COMMAND classes) are stamped by
    :meth:`Producer.publish`.
    """

    subject: str
    event_type: str
    event_class: int  # value from envelope.v1.EventClass
    schema_version: int
    domain: str
    event_time: datetime
    payload_schema_ref: str
    payload: ProtoMessage  # any protobuf message

    ingestion_time: datetime | None = None  # default: now()
    tenant_id: str = ""  # MT-01a; empty ⇒ inherited from ctx, else ProducerConfig.tenant
    correlation_id: str = ""  # empty on roots; Producer fills with event_id
    causation_id: str = ""
    trace_context: str = ""
    partition_key: str = ""
    idempotency_key: str = ""  # required for COMMAND; "" or == event_id otherwise
    quality_flags: list[int] = field(default_factory=list)


class Producer:
    """Stamps + validates envelopes and frames them onto a :class:`Client`.

    Async-safe within a single event loop: the per-(event_type,
    partition_key) sequence counter is updated without a lock because the
    read-modify-write of a dict is atomic in asyncio's cooperative model
    (no ``await`` inside :meth:`_next_sequence`).
    """

    def __init__(self, client: Client, config: ProducerConfig) -> None:
        if client is None:
            raise ValueError("bus: client is required")
        if not config.source:
            raise ValueError("bus: ProducerConfig.source required")
        if not config.producer_version:
            raise ValueError("bus: ProducerConfig.producer_version required")
        self._client = client
        self._config = config
        self._sequence: dict[tuple[str, str], int] = {}

    async def publish(self, event: Event) -> None:
        if event.payload is None:
            raise ValueError("Event.payload required")
        envelope = self._stamp(event)
        validate(envelope)
        payload_bytes = event.payload.SerializeToString()
        frame = EventFrame(envelope=envelope, payload=payload_bytes)
        body = frame.SerializeToString()
        # NATS JetStream keys its broker-side dedup window on Nats-Msg-Id
        # (EVT-08); Kafka treats this as an inert user header. One header
        # serves both transports.
        await self._client.publish(
            Message(
                subject=event.subject,
                body=body,
                key=envelope.partition_key.encode("utf-8") if envelope.partition_key else None,
                headers={"Nats-Msg-Id": envelope.idempotency_key},
            )
        )

    def _stamp(self, e: Event) -> Envelope:
        if e.event_time is None:
            raise ValueError("Event.event_time required")
        event_id = str(uuid7())
        now = datetime.now(timezone.utc)
        ingestion = e.ingestion_time if e.ingestion_time is not None else now

        # Lineage precedence: explicit Event field > contextvar > root default.
        # Consumer stashes inbound envelope fields onto contextvars (EVT-18c)
        # so a handler that publishes a derived event auto-inherits them.
        # MT-01a. Precedence matches lineage: explicit > inherited > configured. A
        # derived event belongs to the tenant of the event that caused it.
        tenant = e.tenant_id or get_tenant_id() or self._config.tenant
        correlation = e.correlation_id or get_correlation_id() or event_id
        causation = e.causation_id or get_causation_id()
        trace = e.trace_context or get_trace_context()

        if e.event_class == EventClass.EVENT_CLASS_COMMAND:
            if not e.idempotency_key:
                raise ValueError("idempotency_key required for COMMAND events")
            idempotency = e.idempotency_key
        else:
            # FACT / STATE_SNAPSHOT / OBSERVATION: idempotency_key = event_id.
            if e.idempotency_key and e.idempotency_key != event_id:
                raise ValueError(
                    "idempotency_key must equal event_id for non-COMMAND events"
                )
            idempotency = event_id

        sequence = self._next_sequence(e.event_type, e.partition_key)

        env = Envelope()
        env.event_id = event_id
        env.event_type = e.event_type
        env.schema_version = e.schema_version
        env.envelope_version = ENVELOPE_VERSION
        env.event_class = e.event_class
        env.domain = e.domain
        env.event_time.FromDatetime(_to_utc(e.event_time))
        env.ingestion_time.FromDatetime(_to_utc(ingestion))
        env.publish_time.FromDatetime(now)
        env.correlation_id = correlation
        env.causation_id = causation
        env.trace_context = trace
        env.tenant_id = tenant
        env.source = self._config.source
        env.producer_version = self._config.producer_version
        env.partition_key = e.partition_key
        env.producer_sequence = sequence
        env.idempotency_key = idempotency
        if e.quality_flags:
            env.quality_flags.extend(e.quality_flags)
        env.payload_schema_ref = e.payload_schema_ref
        return env

    def _next_sequence(self, event_type: str, partition_key: str) -> int:
        if not partition_key:
            return 0  # 0 means N/A per envelope.proto
        key = (event_type, partition_key)
        self._sequence[key] = self._sequence.get(key, 0) + 1
        return self._sequence[key]


def _to_utc(dt: datetime) -> datetime:
    if dt.tzinfo is None:
        return dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)
