"""Envelope-aware consumer wrapper.

Pipeline per delivery::

    Unframe → Validate → dedup-check → retry-loop(handler) → Record on success
                ↘ pre-dispatch              ↘ exhausted ↘
                  DLQ or surface              DLQ or surface

EVT-18a was wire-level; EVT-18b added stamping/validation/unframe; EVT-18c
added lineage propagation via contextvars; EVT-18d (this file) adds dedup,
bounded retry, and DLQ routing. Mirrors the Go ``Consumer`` (kanz/pkg/bus,
EVT-17a–e) so a Python service consuming a topic gives the same observable
behaviour as a Go service.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from typing import Awaitable, Callable

from envelope.v1.envelope_pb2 import Envelope

from kanz_bus.bus import Message, Publisher, Subscriber
from kanz_bus.dedup import DedupWindow
from kanz_bus.frame import unframe
from kanz_bus.propagation import propagation_context
from kanz_bus.retry import DLQ_SUBJECT_PREFIX, RetryConfig
from kanz_bus.validate import validate

EventHandler = Callable[[Envelope, bytes], Awaitable[None]]
"""Async function processing one inbound event.

The handler does *not* receive an explicit context object — lineage rides
:mod:`kanz_bus.propagation` contextvars so any :class:`~kanz_bus.Producer`
publish inside the handler auto-inherits the inbound envelope's
``correlation_id``, ``causation_id`` (set to the inbound ``event_id``), and
``trace_context``. Returning normally acks; raising prevents ack so the
broker redelivers per its transport semantics (unless retry or DLQ
intervenes).
"""


@dataclass
class ConsumerConfig:
    """Composable Consumer options.

    Each field is independent — DLQ-without-retry is "first failure is
    poison"; retry-without-DLQ is "bounded attempts then surface to broker".
    To disable dedup, pass ``DedupWindow(ttl_seconds=0)``.
    """

    dedup_window: DedupWindow | None = None  # None ⇒ default 2m/10k
    retry: RetryConfig = field(default_factory=RetryConfig)
    dlq: Publisher | None = None  # None ⇒ no DLQ, errors surface to broker


class Consumer:
    """Wraps a :class:`Subscriber`.

    Unframe / Validate failures route to DLQ (with ``Kanz-DLQ-Attempts: 0``)
    when configured, else surface so the broker holds the message. Retry
    runs inside the dispatch; a DLQ publish failure surfaces so the broker
    keeps the message and the failure is observable.
    """

    def __init__(self, subscriber: Subscriber, config: ConsumerConfig | None = None) -> None:
        if subscriber is None:
            raise ValueError("bus: subscriber is required")
        config = config or ConsumerConfig()
        self._subscriber = subscriber
        self._dedup = config.dedup_window if config.dedup_window is not None else DedupWindow()
        self._retry = config.retry
        self._max_attempts = max(1, self._retry.max_attempts)
        self._dlq = config.dlq

    async def subscribe(self, subject: str, group: str, handler: EventHandler) -> None:
        async def wrapper(msg: Message) -> None:
            try:
                env, payload = unframe(msg.body)
            except ValueError as e:
                await self._route_predispatch_failure(subject, msg, e)
                return
            try:
                validate(env)
            except ValueError as e:
                await self._route_predispatch_failure(subject, msg, e)
                return

            if self._dedup.seen(env.idempotency_key):
                return  # duplicate within window — already terminally handled

            # causation_id for the *next* event the handler emits is *this*
            # event's event_id — that's how lineage chains.
            with propagation_context(
                correlation_id=env.correlation_id,
                causation_id=env.event_id,
                trace_context=env.trace_context,
            ):
                last_exc: BaseException | None = None
                for attempt in range(1, self._max_attempts + 1):
                    try:
                        await handler(env, payload)
                    except asyncio.CancelledError:
                        raise
                    except Exception as e:
                        last_exc = e
                    else:
                        self._dedup.record(env.idempotency_key)
                        return
                    if attempt < self._max_attempts:
                        await asyncio.sleep(self._retry.backoff(attempt))

            # Retries exhausted.
            assert last_exc is not None
            if self._dlq is not None:
                await self._publish_dlq(subject, msg, self._max_attempts, last_exc)
                self._dedup.record(env.idempotency_key)  # DLQ is terminal
                return
            raise last_exc

        await self._subscriber.subscribe(subject, group, wrapper)

    async def _route_predispatch_failure(
        self, orig_subject: str, msg: Message, exc: Exception
    ) -> None:
        if self._dlq is None:
            raise exc
        await self._publish_dlq(orig_subject, msg, 0, exc)

    async def _publish_dlq(
        self,
        orig_subject: str,
        msg: Message,
        attempts: int,
        exc: BaseException,
    ) -> None:
        headers: dict[str, str] = dict(msg.headers) if msg.headers else {}
        headers["Kanz-DLQ-Original-Subject"] = orig_subject
        headers["Kanz-DLQ-Attempts"] = str(attempts)
        headers["Kanz-DLQ-Error"] = str(exc)
        try:
            await self._dlq.publish(  # type: ignore[union-attr]
                Message(
                    subject=DLQ_SUBJECT_PREFIX + orig_subject,
                    body=msg.body,
                    key=msg.key,
                    headers=headers,
                )
            )
        except Exception as dlq_exc:
            raise RuntimeError(
                f"dlq publish failed: {dlq_exc} (original: {exc})"
            ) from dlq_exc
