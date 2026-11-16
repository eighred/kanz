import asyncio
import os
import time

import pytest
from aiokafka.admin import AIOKafkaAdminClient, NewTopic

from kanz_bus import KafkaClient, Message


@pytest.fixture
def kafka_brokers() -> list[str]:
    raw = os.environ.get("TEST_KAFKA_BROKERS")
    if not raw:
        pytest.skip("TEST_KAFKA_BROKERS not set")
    return raw.split(",")


async def _delete_topic(brokers: list[str], topic: str) -> None:
    admin = AIOKafkaAdminClient(bootstrap_servers=",".join(brokers))
    await admin.start()
    try:
        await admin.delete_topics([topic])
    except Exception:
        pass
    finally:
        await admin.close()


async def test_publish_subscribe(kafka_brokers: list[str]) -> None:
    suffix = str(int(time.time() * 1e9))
    topic = f"test.bus.py.{suffix}"
    group = f"test-consumer-{suffix}"

    # Out-of-band topic provisioning. In production topics are created by
    # kanz/infra/kafka/topics-job.yaml; auto-create is disabled.
    admin = AIOKafkaAdminClient(bootstrap_servers=",".join(kafka_brokers))
    await admin.start()
    try:
        await admin.create_topics(
            [NewTopic(name=topic, num_partitions=1, replication_factor=1)]
        )
    finally:
        await admin.close()

    try:
        async with KafkaClient(brokers=kafka_brokers, client_id="bus-test") as client:
            received: asyncio.Queue[Message] = asyncio.Queue()

            async def handler(msg: Message) -> None:
                await received.put(msg)

            sub_task = asyncio.create_task(client.subscribe(topic, group, handler))
            await asyncio.sleep(2.0)  # let consumer join the group

            await client.publish(
                Message(
                    subject=topic,
                    body=b"hello",
                    key=b"partition-1",
                    headers={"X-Test": "true"},
                )
            )

            got = await asyncio.wait_for(received.get(), timeout=15.0)
            assert got.body == b"hello"
            assert got.key == b"partition-1"
            assert got.headers and got.headers.get("X-Test") == "true"

            sub_task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await sub_task
    finally:
        await _delete_topic(kafka_brokers, topic)
