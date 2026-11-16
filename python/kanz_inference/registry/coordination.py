"""Coordinated model registry (DEBT-02b).

The PRED-09 ``Registry`` is process-local by design — each worker has its own. A
multi-replica inference fleet then has no shared view: a model promoted on one
pod is invisible to the others until a config push + rolling restart. DEBT-02b
closes that by backing registration on the ``platform.model`` topic:

  - a mutation (register / record_validation / unregister) is applied locally
    AND published as a ``RegistryEvent`` to ``platform.model``;
  - every replica consumes ``platform.model`` and ``apply``s inbound events to
    its local Registry, so the fleet converges.

``platform.model`` is the append log of record (infinite retention, EVT-09), so
a starting replica **replays it from the beginning to rebuild the registry** —
the "+ cache" half: the topic is the source of truth, the local Registry is the
materialized cache.

# Why events carry metadata, not Model instances

A ``Model`` is a live object (loaded weights), not serializable. The event
carries ``ModelMetadata`` + role + validation; ``apply`` rematerializes the
``Model`` locally via the injected ``ModelLoader`` (the same seam PRED-09 already
defines). So the wire stays small and language-agnostic.

# Ordering + the primary gate

Publish ``platform.model`` keyed by ``model_id`` so a model's record_validation
and its primary register stay in per-partition order — every replica applies the
validation before the promote, satisfying the MLOPS-01a gate identically. A
register whose gate fails on apply is surfaced (the same ``ValidationError`` the
local path raises), not silently dropped, so a divergence is visible.

# Loop prevention

Each replica stamps its ``origin`` on events it publishes and skips applying its
own (it already did the work locally) — so the publish→consume round trip
doesn't double-apply.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Protocol

from kanz_inference.registry.registry import (
    ModelLoader,
    ModelMetadata,
    Registry,
    ValidationRecord,
)
from kanz_inference.streaming.worker import Model

# Op names — the discriminator on RegistryEvent.
OP_REGISTER = "register"
OP_UNREGISTER = "unregister"
OP_RECORD_VALIDATION = "record_validation"


@dataclass(frozen=True)
class RegistryEvent:
    """A serializable model-registry mutation, published to ``platform.model``
    and applied by every replica. One shape covers all three ops; fields not
    relevant to an op are left at their defaults."""

    op: str
    origin: str
    """The replica that produced this event — used to skip self-apply."""

    # --- register / unregister ---
    model_id: str = ""
    feature_set_ref: str = ""
    confidence_threshold: float = 0.0
    artifact_uri: str = ""
    primary: bool = True

    # --- record_validation ---
    validated_at: datetime | None = None
    expires_at: datetime | None = None
    passed: bool = True
    report_uri: str = ""

    def to_dict(self) -> dict[str, Any]:
        """Wire form — JSON-friendly (datetimes as ISO-8601). The publisher
        serializes this onto the topic; from_dict reverses it on consume."""
        d: dict[str, Any] = {
            "op": self.op,
            "origin": self.origin,
            "model_id": self.model_id,
            "feature_set_ref": self.feature_set_ref,
            "confidence_threshold": self.confidence_threshold,
            "artifact_uri": self.artifact_uri,
            "primary": self.primary,
            "passed": self.passed,
            "report_uri": self.report_uri,
        }
        if self.validated_at is not None:
            d["validated_at"] = self.validated_at.isoformat()
        if self.expires_at is not None:
            d["expires_at"] = self.expires_at.isoformat()
        return d

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> RegistryEvent:
        def _ts(key: str) -> datetime | None:
            v = d.get(key)
            return datetime.fromisoformat(v) if v else None

        return cls(
            op=d["op"],
            origin=d.get("origin", ""),
            model_id=d.get("model_id", ""),
            feature_set_ref=d.get("feature_set_ref", ""),
            confidence_threshold=d.get("confidence_threshold", 0.0),
            artifact_uri=d.get("artifact_uri", ""),
            primary=d.get("primary", True),
            validated_at=_ts("validated_at"),
            expires_at=_ts("expires_at"),
            passed=d.get("passed", True),
            report_uri=d.get("report_uri", ""),
        )


class RegistryPublisher(Protocol):
    """The ``platform.model`` publish seam. The deployment wires this over the
    bus client (Kafka/NATS); the registry stays decoupled from the transport,
    the same stance as the Go bus' injectable clients."""

    async def publish(self, event: RegistryEvent) -> None: ...


class CoordinatedRegistry:
    """Wraps a process-local :class:`Registry`, mirroring every mutation onto
    ``platform.model`` and applying inbound events from peers. Reads delegate to
    the local registry (``primary_for_feature_set`` etc.) via :attr:`local`.
    """

    def __init__(
        self,
        *,
        local: Registry,
        publisher: RegistryPublisher,
        loader: ModelLoader,
        origin: str,
    ) -> None:
        if not origin:
            raise ValueError("origin required (replica identity for loop prevention)")
        self.local = local
        self._publisher = publisher
        self._loader = loader
        self._origin = origin

    async def register(
        self, metadata: ModelMetadata, model: Model, primary: bool = True
    ) -> None:
        """Register locally (running the MLOPS-01a primary gate), then publish so
        peers converge. Publish happens only after a successful local register,
        so a gate rejection never propagates."""
        self.local.register(metadata, model, primary=primary)
        await self._publisher.publish(
            RegistryEvent(
                op=OP_REGISTER,
                origin=self._origin,
                model_id=metadata.model_id,
                feature_set_ref=metadata.feature_set_ref,
                confidence_threshold=metadata.confidence_threshold,
                artifact_uri=metadata.artifact_uri,
                primary=primary,
            )
        )

    async def record_validation(self, record: ValidationRecord) -> None:
        """Record locally, then publish so peers can satisfy the primary gate
        for the subsequent promote (ordered per-partition by model_id)."""
        self.local.record_validation(record)
        await self._publisher.publish(
            RegistryEvent(
                op=OP_RECORD_VALIDATION,
                origin=self._origin,
                model_id=record.model_id,
                validated_at=record.validated_at,
                expires_at=record.expires_at,
                passed=record.passed,
                report_uri=record.report_uri,
            )
        )

    async def unregister(self, model_id: str) -> bool:
        """Unregister locally; publish only if it was present (no-op events add
        nothing). Returns whether the model was present locally."""
        removed = self.local.unregister(model_id)
        if removed:
            await self._publisher.publish(
                RegistryEvent(op=OP_UNREGISTER, origin=self._origin, model_id=model_id)
            )
        return removed

    async def apply(self, event: RegistryEvent) -> None:
        """Apply an inbound ``platform.model`` event to the local registry. The
        ``platform.model`` consumer calls this for every delivered event,
        including the replay from offset 0 that warms the cache on startup.

        Self-originated events are skipped (already applied locally). A register
        rematerializes the Model via the loader; a primary register still passes
        the local MLOPS-01a gate (the validation event precedes it in order)."""
        if event.origin == self._origin:
            return
        if event.op == OP_RECORD_VALIDATION:
            self.local.record_validation(
                ValidationRecord(
                    model_id=event.model_id,
                    validated_at=event.validated_at or datetime.now(timezone.utc),
                    expires_at=event.expires_at or datetime.now(timezone.utc),
                    passed=event.passed,
                    report_uri=event.report_uri,
                )
            )
        elif event.op == OP_REGISTER:
            metadata = ModelMetadata(
                model_id=event.model_id,
                feature_set_ref=event.feature_set_ref,
                confidence_threshold=event.confidence_threshold,
                artifact_uri=event.artifact_uri,
            )
            model = await self._loader.load(metadata)
            self.local.register(metadata, model, primary=event.primary)
        elif event.op == OP_UNREGISTER:
            self.local.unregister(event.model_id)
        else:
            raise ValueError(f"unknown registry event op: {event.op!r}")
