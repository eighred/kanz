"""NATS JetStream wrapper — connection mgmt + publish/subscribe."""

from __future__ import annotations

import asyncio
from typing import Any

import nats
from nats.aio.client import Client as NATSConn
from nats.js import JetStreamContext

from kanz_bus.bus import NATS_PARTITION_KEY_HEADER, Handler, Message


class NATSClient:
    """Bus client over NATS JetStream.

    Use as an async context manager::

        async with NATSClient(url="nats://localhost:4222") as client:
            await client.publish(Message(subject="order.order.submit", body=b"..."))

    Or call :meth:`connect` and :meth:`close` directly. Re-calling
    :meth:`connect` on a connected client is a no-op.
    """

    def __init__(
        self,
        url: str,
        name: str = "kanz",
        connect_timeout: float = 10.0,
        reconnect_time_wait: float = 2.0,
        max_reconnect_attempts: int = -1,
        publish_timeout: float = 5.0,
    ) -> None:
        if not url:
            raise ValueError("nats: url required")
        self._url = url
        self._name = name
        self._connect_timeout = connect_timeout
        self._reconnect_time_wait = reconnect_time_wait
        self._max_reconnect_attempts = max_reconnect_attempts
        self._publish_timeout = publish_timeout
        self._nc: NATSConn | None = None
        self._js: JetStreamContext | None = None

    async def connect(self) -> None:
        if self._nc is not None:
            return
        self._nc = await nats.connect(
            self._url,
            name=self._name,
            connect_timeout=self._connect_timeout,
            reconnect_time_wait=self._reconnect_time_wait,
            max_reconnect_attempts=self._max_reconnect_attempts,
        )
        self._js = self._nc.jetstream()

    async def close(self) -> None:
        if self._nc is not None:
            await self._nc.close()
            self._nc = None
            self._js = None

    async def __aenter__(self) -> "NATSClient":
        await self.connect()
        return self

    async def __aexit__(self, *exc: Any) -> None:
        await self.close()

    async def publish(self, msg: Message) -> None:
        if self._js is None:
            raise RuntimeError("nats: client not connected; call connect() or use `async with`")
        headers: dict[str, str] = dict(msg.headers) if msg.headers else {}
        if msg.key:
            headers[NATS_PARTITION_KEY_HEADER] = msg.key.decode("utf-8")
        await asyncio.wait_for(
            self._js.publish(msg.subject, msg.body, headers=headers or None),
            timeout=self._publish_timeout,
        )

    async def subscribe(self, subject: str, group: str, handler: Handler) -> None:
        """Pull-subscribe to ``subject`` under durable consumer ``group``.

        Blocks until the surrounding task is canceled. Handler exceptions
        (other than :class:`asyncio.CancelledError`) trigger ``nak`` so the
        broker redelivers; normal return triggers ``ack``. JetStream stream
        binding is provisioned out-of-band by ``kanz/infra/nats/``.
        """
        if self._js is None:
            raise RuntimeError("nats: client not connected")
        psub = await self._js.pull_subscribe(subject, durable=group)
        try:
            while True:
                try:
                    msgs = await psub.fetch(batch=1, timeout=10)
                except asyncio.TimeoutError:
                    continue
                for raw in msgs:
                    msg = _nats_to_message(raw)
                    try:
                        await handler(msg)
                    except Exception:
                        await raw.nak()
                        continue
                    await raw.ack()
        except asyncio.CancelledError:
            try:
                await psub.unsubscribe()
            except Exception:
                pass
            raise


def _nats_to_message(raw: Any) -> Message:
    headers: dict[str, str] = dict(raw.headers) if raw.headers else {}
    key_str = headers.pop(NATS_PARTITION_KEY_HEADER, None)
    key = key_str.encode("utf-8") if key_str else None
    return Message(
        subject=raw.subject,
        body=raw.data,
        key=key,
        headers=headers or None,
    )
