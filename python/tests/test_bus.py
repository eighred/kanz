import pytest

from kanz_bus import KafkaClient, Message, NATSClient


def test_message_dataclass_defaults():
    m = Message(subject="x")
    assert m.subject == "x"
    assert m.body == b""
    assert m.key is None
    assert m.headers is None


def test_message_explicit_fields():
    m = Message(subject="x", body=b"data", key=b"k", headers={"a": "b"})
    assert m.body == b"data"
    assert m.key == b"k"
    assert m.headers == {"a": "b"}


def test_nats_client_rejects_empty_url():
    with pytest.raises(ValueError):
        NATSClient(url="")


def test_kafka_client_rejects_empty_brokers():
    with pytest.raises(ValueError):
        KafkaClient(brokers=[])


async def test_nats_publish_before_connect_raises():
    client = NATSClient(url="nats://localhost:4222")
    with pytest.raises(RuntimeError):
        await client.publish(Message(subject="x", body=b"hi"))


async def test_kafka_publish_before_connect_raises():
    client = KafkaClient(brokers=["localhost:9092"])
    with pytest.raises(RuntimeError):
        await client.publish(Message(subject="x", body=b"hi"))


async def test_nats_close_without_connect_is_noop():
    client = NATSClient(url="nats://localhost:4222")
    await client.close()  # must not raise


async def test_kafka_close_without_connect_is_noop():
    client = KafkaClient(brokers=["localhost:9092"])
    await client.close()  # must not raise
