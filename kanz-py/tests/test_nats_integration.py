import asyncio
import os
import time

import nats
import pytest
from nats.js.api import RetentionPolicy, StorageType, StreamConfig

from kanz_bus import Message, NATSClient


@pytest.fixture
def nats_url() -> str:
    url = os.environ.get("TEST_NATS_URL")
    if not url:
        pytest.skip("TEST_NATS_URL not set")
    return url


async def test_publish_subscribe(nats_url: str) -> None:
    suffix = str(int(time.time() * 1e9))
    stream_name = f"TEST_BUS_PY_{suffix}"
    subject = f"test.bus.py.{suffix}"

    # Out-of-band stream provisioning. The bus client itself never manages
    # streams — in production they're provisioned via kanz/infra/nats/.
    setup_nc = await nats.connect(nats_url)
    setup_js = setup_nc.jetstream()
    await setup_js.add_stream(
        StreamConfig(
            name=stream_name,
            subjects=[subject],
            storage=StorageType.MEMORY,
            retention=RetentionPolicy.LIMITS,
        )
    )
    try:
        async with NATSClient(url=nats_url, name="bus-test") as client:
            received: asyncio.Queue[Message] = asyncio.Queue()

            async def handler(msg: Message) -> None:
                await received.put(msg)

            sub_task = asyncio.create_task(
                client.subscribe(subject, f"test-consumer-{suffix}", handler)
            )
            await asyncio.sleep(0.5)  # let the consumer bind

            await client.publish(
                Message(
                    subject=subject,
                    body=b"hello",
                    key=b"partition-1",
                    headers={"X-Test": "true"},
                )
            )

            got = await asyncio.wait_for(received.get(), timeout=5.0)
            assert got.body == b"hello"
            assert got.key == b"partition-1"
            assert got.headers and got.headers.get("X-Test") == "true"
            # Kanz-Partition-Key must not leak into headers — surfaced as key.
            assert "Kanz-Partition-Key" not in (got.headers or {})

            sub_task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await sub_task
    finally:
        try:
            await setup_js.delete_stream(stream_name)
        finally:
            await setup_nc.close()
