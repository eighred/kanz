"""Model-registry publish tests (DEBT-02b, #112).

Tier-B, like the prediction publish tests beside them: a REAL ``kanz_bus.Producer``
over a capturing transport, so stamping, ``validate`` and framing all really run
and the only thing mocked out is the socket.

That matters more here than usual. ``RegistryPublisher`` was a Protocol with no
implementation, so every existing test of ``CoordinatedRegistry`` ran against a
double that accepted anything — which is precisely how a publisher ships with an
envelope a real broker refuses.
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from envelope.v1.event_class_pb2 import EventClass
from inference.v1.model_registry_pb2 import (
    ModelRegistryEvent,
    ModelRegistryOp,
    ModelRole,
)

from kanz_bus import Message, Producer, ProducerConfig, unframe, validate
from kanz_inference.registry.coordination import (
    OP_RECORD_VALIDATION,
    OP_REGISTER,
    OP_UNREGISTER,
    RegistryEvent,
)
from kanz_inference.registry.publish import (
    SCHEMA_REF_MODEL_REGISTRY,
    SUBJECT_MODEL_REGISTERED,
    BusRegistryPublisher,
)


class CaptureClient:
    """Bus client that records every publish. Sits BELOW the Producer, so
    Validate is on the path."""

    def __init__(self) -> None:
        self.sent: list[Message] = []

    async def publish(self, message: Message) -> None:
        self.sent.append(message)

    async def subscribe(self, subject, group, handler):
        raise NotImplementedError("not used in publish tests")

    async def close(self) -> None:
        pass


def _publisher() -> tuple[BusRegistryPublisher, CaptureClient]:
    client = CaptureClient()
    producer = Producer(
        client,
        ProducerConfig(source="inference-svc/test", producer_version="inf-1.0.0", tenant="acme"),
    )
    return BusRegistryPublisher(producer), client


def _register(**kw) -> RegistryEvent:
    base = dict(
        op=OP_REGISTER,
        origin="pod-a",
        model_id="risk-v3",
        feature_set_ref="portfolio-risk:1",
        confidence_threshold=0.7,
        artifact_uri="s3://models/risk-v3",
        primary=True,
    )
    base.update(kw)
    return RegistryEvent(**base)


def _payload_of(msg: Message) -> ModelRegistryEvent:
    env, raw = unframe(msg.body)
    validate(env)  # raises on contract violation — a real broker would too
    payload = ModelRegistryEvent()
    payload.ParseFromString(raw)
    return payload


# --- The envelope a real broker accepts -------------------------------


@pytest.mark.asyncio
async def test_register_emits_a_valid_fact_envelope():
    pub, client = _publisher()
    await pub.publish(_register())

    assert len(client.sent) == 1
    msg = client.sent[0]
    assert msg.subject == SUBJECT_MODEL_REGISTERED

    # KEYED BY model_id. Without this a model's validation and its promotion can
    # land on different partitions, and a replica applies the promotion first —
    # the MLOPS-01a gate then refuses it on some replicas and not others.
    assert msg.key == b"risk-v3"

    env, _ = unframe(msg.body)
    validate(env)
    assert env.event_class == EventClass.EVENT_CLASS_FACT
    assert env.domain == "platform"
    assert env.payload_schema_ref == SCHEMA_REF_MODEL_REGISTRY
    assert env.tenant_id == "acme"
    assert env.event_id


@pytest.mark.asyncio
async def test_the_op_and_the_role_reach_the_wire():
    pub, client = _publisher()
    await pub.publish(_register(primary=True))
    payload = _payload_of(client.sent[0])
    assert payload.op == ModelRegistryOp.MODEL_REGISTRY_OP_REGISTER
    assert payload.role == ModelRole.MODEL_ROLE_PRIMARY
    assert payload.model_id == "risk-v3"
    assert payload.origin == "pod-a"

    # THE FEATURE CONTRACT MUST SURVIVE. Resolution is by feature_set_ref and
    # never by name (#492); a model whose contract is lost on the wire is
    # resolvable only by name on every replica that rebuilt from the log.
    assert payload.feature_set_ref == "portfolio-risk:1"
    assert payload.confidence_threshold == pytest.approx(0.7)
    assert payload.artifact_uri == "s3://models/risk-v3"


@pytest.mark.asyncio
async def test_a_candidate_is_not_published_as_primary():
    pub, client = _publisher()
    await pub.publish(_register(primary=False))
    assert _payload_of(client.sent[0]).role == ModelRole.MODEL_ROLE_CANDIDATE


@pytest.mark.asyncio
async def test_unregister_carries_its_op():
    pub, client = _publisher()
    await pub.publish(_register(op=OP_UNREGISTER))
    assert _payload_of(client.sent[0]).op == ModelRegistryOp.MODEL_REGISTRY_OP_UNREGISTER


# --- Validation evidence ----------------------------------------------


@pytest.mark.asyncio
async def test_record_validation_carries_its_timestamps():
    validated = datetime(2026, 8, 15, 12, 0, 0, tzinfo=timezone.utc)
    expires = validated + timedelta(days=90)
    pub, client = _publisher()
    await pub.publish(
        _register(op=OP_RECORD_VALIDATION, validated_at=validated, expires_at=expires, passed=True)
    )
    payload = _payload_of(client.sent[0])
    assert payload.op == ModelRegistryOp.MODEL_REGISTRY_OP_RECORD_VALIDATION
    assert payload.validated_at.ToDatetime(tzinfo=timezone.utc) == validated

    # THE EXPIRY IS THE POINT. SR 11-7 is about CURRENT validation, so a
    # validation published without one is a sign-off that never lapses.
    assert payload.expires_at.ToDatetime(tzinfo=timezone.utc) == expires


@pytest.mark.asyncio
async def test_a_failed_validation_is_still_published():
    """Dropping it would make 'graded and rejected' and 'never graded' the same
    observable state on every replica that rebuilt from the log."""
    pub, client = _publisher()
    await pub.publish(
        _register(
            op=OP_RECORD_VALIDATION,
            validated_at=datetime(2026, 8, 15, tzinfo=timezone.utc),
            passed=False,
        )
    )
    assert len(client.sent) == 1
    assert _payload_of(client.sent[0]).passed is False


@pytest.mark.asyncio
async def test_a_naive_timestamp_is_read_as_utc_not_local():
    """A naive datetime is interpreted as LOCAL time by FromDatetime, so the same
    validation published from two timezones would land at two different instants
    — and the expiry the gate reads would move with it."""
    naive = datetime(2026, 8, 15, 12, 0, 0)  # no tzinfo
    pub, client = _publisher()
    await pub.publish(_register(op=OP_RECORD_VALIDATION, validated_at=naive))
    got = _payload_of(client.sent[0]).validated_at.ToDatetime(tzinfo=timezone.utc)
    assert got == datetime(2026, 8, 15, 12, 0, 0, tzinfo=timezone.utc)


# --- What is refused, and why -----------------------------------------


@pytest.mark.asyncio
async def test_an_event_without_a_model_id_is_refused():
    """model_id is the partition key. Without it the ordering the gate depends on
    does not exist, and the event goes to an arbitrary partition."""
    pub, client = _publisher()
    with pytest.raises(ValueError, match="partition key"):
        await pub.publish(_register(model_id=""))
    assert client.sent == []


@pytest.mark.asyncio
async def test_an_event_without_an_origin_is_refused():
    """origin is what stops a replica applying its own event twice. Empty makes
    every replica treat it as someone else's, and a promotion applied a second
    time re-runs the gate against state that has already moved."""
    pub, client = _publisher()
    with pytest.raises(ValueError, match="loop prevention"):
        await pub.publish(_register(origin=""))
    assert client.sent == []


@pytest.mark.asyncio
async def test_an_unknown_op_is_refused_rather_than_sent_as_unspecified():
    """An op added on one side and not the other must fail at the publish site.
    Sent as UNSPECIFIED it would be silently ignored by every consumer, and the
    mutation would be lost from a log that is the source of truth."""
    pub, client = _publisher()
    with pytest.raises(ValueError, match="unknown op"):
        await pub.publish(_register(op="promote_somehow"))
    assert client.sent == []
