# kanz-bus — Python bus client

Python bus client for the Kanz event backbone. Wraps NATS JetStream and
Kafka with a uniform async publish/subscribe API. Mirrors the Go client at
`kanz/pkg/bus` (EVT-17) on the wire so producers/consumers in either
language interoperate.

## Status

| Subtask | Scope |
|---|---|
| **EVT-18a** | Wire-level publish/subscribe wrappers (NATS + Kafka) — **done** |
| **EVT-18b** | Envelope stamping + pre-publish validation — **done** |
| **EVT-18c** | Trace / correlation / causation propagation — **done** |
| **EVT-18d** | Dedup + retry + DLQ routing — **done** |

**EVT-18b** prerequisite: the `kanz-schemas` Python SDK must be installable.
Until the first kanz-schemas release tag, generate locally:

```sh
cd ../kanz-schemas && buf generate
pip install -e ../kanz-schemas/gen/python
```

## Install (dev)

```sh
cd kanz-py
pip install -e ".[test]"
```

## Test

```sh
pytest                                                    # unit tests
TEST_NATS_URL=nats://localhost:4222 pytest                # + NATS integration
TEST_KAFKA_BROKERS=localhost:9092 pytest                  # + Kafka integration
```

## Usage

```python
import asyncio
from kanz_bus import Message, NATSClient


async def main() -> None:
    async with NATSClient(url="nats://localhost:4222") as client:
        # Publish
        await client.publish(Message(subject="market.equity.trade", body=b"..."))

        # Subscribe (blocks until the task is canceled)
        async def handler(msg: Message) -> None:
            print(msg.subject, msg.body)

        await client.subscribe(
            "market.equity.trade",
            "my-consumer-group",
            handler,
        )


asyncio.run(main())
```

`KafkaClient` is the same shape with `brokers=["host:port", …]` in place of
`url=`.
