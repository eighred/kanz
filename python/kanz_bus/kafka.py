"""Kafka wrapper — connection mgmt + publish/subscribe via aiokafka."""

from __future__ import annotations

import asyncio
from typing import Any

from aiokafka import AIOKafkaConsumer, AIOKafkaProducer

from kanz_bus.bus import Handler, Message


class KafkaClient:
    """Bus client over Kafka (aiokafka).

    Use as an async context manager::

        async with KafkaClient(brokers=["localhost:9092"]) as client:
            await client.publish(Message(subject="market.equity", key=b"AAPL", body=b"..."))
    """

    def __init__(
        self,
        brokers: list[str],
        client_id: str = "kanz",
        publish_timeout: float = 5.0,
    ) -> None:
        if not brokers:
            raise ValueError("kafka: at least one broker required")
        self._brokers = brokers
        self._client_id = client_id
        self._publish_timeout = publish_timeout
        self._producer: AIOKafkaProducer | None = None

    async def connect(self) -> None:
        if self._producer is not None:
            return
        # acks="all" matches the Go writer's RequireAll. Topic provisioning
        # is broker-side (EVT-09: auto-create disabled, topics-job owns it),
        # so we don't pass an auto-create flag.
        self._producer = AIOKafkaProducer(
            bootstrap_servers=",".join(self._brokers),
            client_id=self._client_id,
            acks="all",
            compression_type="snappy",
        )
        await self._producer.start()

    async def close(self) -> None:
        if self._producer is not None:
            await self._producer.stop()
            self._producer = None

    async def __aenter__(self) -> "KafkaClient":
        await self.connect()
        return self

    async def __aexit__(self, *exc: Any) -> None:
        await self.close()

    async def publish(self, msg: Message) -> None:
        if self._producer is None:
            raise RuntimeError("kafka: client not connected; call connect() or use `async with`")
        kafka_headers: list[tuple[str, bytes]] = []
        if msg.headers:
            kafka_headers = [(k, v.encode("utf-8")) for k, v in msg.headers.items()]
        await asyncio.wait_for(
            self._producer.send_and_wait(
                msg.subject,
                value=msg.body,
                key=msg.key,
                headers=kafka_headers,
            ),
            timeout=self._publish_timeout,
        )

    async def subscribe(self, topic: str, group: str, handler: Handler) -> None:
        """Consume ``topic`` under consumer-group ``group``.

        Blocks until the surrounding task is canceled. Handler exceptions
        (other than :class:`asyncio.CancelledError`) skip the commit so the
        message is redelivered on next fetch; normal return commits.
        Bounded retry + DLQ routing layer on top in EVT-18d.
        """
        consumer = AIOKafkaConsumer(
            topic,
            bootstrap_servers=",".join(self._brokers),
            group_id=group,
            client_id=self._client_id,
            enable_auto_commit=False,
        )
        await consumer.start()
        try:
            async for raw in consumer:
                msg = _kafka_to_message(raw)
                try:
                    await handler(msg)
                except Exception:
                    # No commit — message is redelivered on next fetch.
                    continue
                await consumer.commit()
        finally:
            await consumer.stop()


def _kafka_to_message(raw: Any) -> Message:
    headers: dict[str, str] = {}
    if raw.headers:
        for k, v in raw.headers:
            headers[k] = v.decode("utf-8") if v else ""
    return Message(
        subject=raw.topic,
        body=raw.value or b"",
        key=raw.key,
        headers=headers or None,
    )
