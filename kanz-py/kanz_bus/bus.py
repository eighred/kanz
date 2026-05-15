"""kanz_bus — Python bus client for the Kanz event backbone.

EVT-18a is the wire-level layer only: connect, publish bytes, subscribe to
bytes. EVT-18b adds envelope stamping + pre-publish validation; EVT-18c
adds trace / correlation / causation propagation; EVT-18d adds dedup +
DLQ routing. Mirrors the Go client (kanz/pkg/bus, EVT-17) on the wire so
producers/consumers in either language interoperate.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Awaitable, Callable, Protocol


@dataclass
class Message:
    """One wire-level event: opaque body plus transport-agnostic metadata.

    Body is typically a serialized envelope; later EVT-18 layers stamp the
    envelope before calling :meth:`Publisher.publish`.

    Attributes:
        subject: NATS subject / Kafka topic per kanz-schemas/docs/subject-taxonomy.md.
        body: Opaque payload — typically a serialized envelope frame.
        key: Partition key. Used directly as the Kafka message key. NATS has
            no native partition key — wrappers carry it as the
            ``Kanz-Partition-Key`` header on the wire and reconstruct
            ``key`` on receive.
        headers: Free-form transport metadata (trace ids, schema refs, …).
    """

    subject: str
    body: bytes = b""
    key: bytes | None = None
    headers: dict[str, str] | None = None


Handler = Callable[[Message], Awaitable[None]]
"""Async function processing one inbound :class:`Message`.

Returning normally acks/commits the message; raising an exception (other
than :class:`asyncio.CancelledError`, which propagates cancellation) prevents
ack so the broker redelivers per its transport semantics. Bounded retry +
DLQ routing layer on top in EVT-18d.
"""


class Publisher(Protocol):
    """Publishes one message at a time. Errors are transport-level."""

    async def publish(self, msg: Message) -> None: ...


class Subscriber(Protocol):
    """Subscribes a :data:`Handler` to (subject, group).

    ``subscribe`` blocks until the task is canceled
    (``asyncio.CancelledError`` propagates out). ``group`` is the durable
    consumer name (NATS) / consumer-group id (Kafka).
    """

    async def subscribe(self, subject: str, group: str, handler: Handler) -> None: ...


class Client(Publisher, Subscriber, Protocol):
    """Transport that does both. NATSClient and KafkaClient satisfy it."""

    async def close(self) -> None: ...


NATS_PARTITION_KEY_HEADER = "Kanz-Partition-Key"
"""Wire header carrying :attr:`Message.key` over NATS, which lacks a native
partition-key concept (Kafka has one). The receive path strips this header
from the user-visible :attr:`Message.headers` and surfaces it as
:attr:`Message.key`. Matches the Go client (``kanz/pkg/bus``, EVT-17a)."""
