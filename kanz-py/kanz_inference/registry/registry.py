"""Model registry — versioned in-memory mapping of model_id +
feature_set_ref → (metadata, Model instance).

# Why primary/shadow at registration time

Two models can serve the same feature_set_ref simultaneously: the
PRIMARY produces the response served to consumers; the SHADOWS run
in parallel for evaluation (PRED-10). The registry needs to
discriminate them so the workers can fan out shadow traffic without
mixing model versions into the served response.

The alternative — single map, caller picks the "right" one — would
spread routing policy across every caller. Explicit
primary/shadow distinction at registration time concentrates the
policy in one place (whoever calls register) and gives the workers
two simple, intent-revealing lookup methods.

# What the registry does NOT do

- Artifact LOADING. The registry stores constructed Model
  instances; how an artifact_uri becomes a Model is a deployment
  concern that plugs in via ``ModelLoader``. Reasons: loading
  semantics differ wildly across model formats (pickle, ONNX,
  torchscript, joblib, remote inference server), and PRED-09's job
  is the lookup contract, not the format-specific deserialisation.
- Lifecycle (warmup, eviction). A long-running worker keeps every
  registered model in memory; if memory pressure becomes real,
  PRED-09b can add an LRU layer.
- Distributed state. The registry is process-local — each worker
  process has its own. Cross-process model-version coordination
  is a deployment problem (config management + rolling restarts),
  not a runtime registry concern.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Protocol

from kanz_inference.streaming.worker import Model


@dataclass(frozen=True)
class ModelMetadata:
    """Per-model contract — what the registry needs to know about
    one Model instance.

    ``frozen=True`` so it's safely hashable and can be a dict key
    in higher-level structures.
    """

    model_id: str
    """Canonical id, format ``"{name}@{version}"`` (e.g.
    ``"vol-forecast@1.4.2"``). Same shape as
    PredictionEnvelope.model and observation.v1.DecisionLog.decider."""

    feature_set_ref: str
    """The feature catalog version this model consumes, format
    ``"{name}:{version}"`` (matches FeatureVector.feature_set_ref).
    The routing key: workers find a model by looking up the
    primary for the inbound feature event's feature_set_ref."""

    confidence_threshold: float
    """Per PRED-02 §2.3, predictions with confidence below this are
    DEGRADED. The threshold is the MODEL's contract, not a global
    constant — a high-conviction signal model picks 0.8; a
    probability-of-default model picks 0.3. Models without
    calibrated confidence set this to 1.0 so every prediction is
    marked DEGRADED (PRED-02 "unknown confidence as no confidence"
    discipline)."""

    artifact_uri: str
    """Where the model artifact came from (e.g.
    ``"s3://kanz-models/vol-forecast/1.4.2/model.pkl"``). For
    audit / traceability only — the registry does not interpret
    or fetch the URI itself; that's the loader's job."""


class ModelLoader(Protocol):
    """Optional pluggable artifact loader. Implementations turn a
    ModelMetadata.artifact_uri into a Model instance ready to call
    ``predict``. The protocol exists so deployment-specific loading
    (pickle on disk, ONNX runtime, remote model server) plugs in
    without changing the registry contract.
    """

    async def load(self, metadata: ModelMetadata) -> Model:
        ...


class Registry:
    """In-memory model registry. Single process; not safe to share
    across processes (which is fine — each worker has its own).

    Thread-safety: single asyncio event loop only. Registration is
    typically once-at-startup and rare-on-rollout; concurrent
    register calls under threads would race the inner dicts.
    """

    def __init__(self) -> None:
        # Single map by model_id, plus index by feature_set_ref →
        # (primary_model_id, [shadow_model_ids]). The redundancy
        # keeps both lookups O(1) without iterating.
        self._by_id: dict[str, tuple[ModelMetadata, Model]] = {}
        self._primary_for_fs: dict[str, str] = {}
        self._shadows_for_fs: dict[str, list[str]] = {}

    def register(
        self,
        metadata: ModelMetadata,
        model: Model,
        primary: bool = True,
    ) -> None:
        """Register a Model instance with its metadata.

        ``primary=True`` (default) makes the model the active
        serving model for its ``feature_set_ref`` — replacing any
        previously-primary model. ``primary=False`` adds it as a
        shadow for that feature_set (PRED-10).

        Registering the same ``model_id`` twice replaces the
        entry. Use this for hot-reload of a model artifact.
        """
        if not metadata.model_id:
            raise ValueError("ModelMetadata.model_id required")
        if not metadata.feature_set_ref:
            raise ValueError("ModelMetadata.feature_set_ref required")
        if "@" not in metadata.model_id:
            raise ValueError(
                f'ModelMetadata.model_id must be "name@version", got {metadata.model_id!r}'
            )
        if ":" not in metadata.feature_set_ref:
            raise ValueError(
                f'ModelMetadata.feature_set_ref must be "name:version", got {metadata.feature_set_ref!r}'
            )
        if model is None:
            raise ValueError("model required")

        # If re-registering an existing model_id, scrub its old
        # role from the per-feature-set indexes so a "primary →
        # shadow" demotion (or vice versa) is consistent.
        if metadata.model_id in self._by_id:
            self._remove_from_role_indexes(metadata.model_id)

        self._by_id[metadata.model_id] = (metadata, model)
        if primary:
            self._primary_for_fs[metadata.feature_set_ref] = metadata.model_id
        else:
            shadows = self._shadows_for_fs.setdefault(metadata.feature_set_ref, [])
            if metadata.model_id not in shadows:
                shadows.append(metadata.model_id)

    def get_by_id(self, model_id: str) -> tuple[ModelMetadata, Model] | None:
        """Look up by canonical id. Returns ``(metadata, model)``
        or ``None``."""
        return self._by_id.get(model_id)

    def primary_for_feature_set(
        self, feature_set_ref: str
    ) -> tuple[ModelMetadata, Model] | None:
        """The model that serves responses for inbound features
        with this feature_set_ref. ``None`` if no primary is
        registered for the feature set."""
        model_id = self._primary_for_fs.get(feature_set_ref)
        if model_id is None:
            return None
        return self._by_id.get(model_id)

    def shadows_for_feature_set(
        self, feature_set_ref: str
    ) -> list[tuple[ModelMetadata, Model]]:
        """All shadow models for this feature_set_ref. PRED-10
        fans inbound features out to every shadow for parallel
        evaluation; the primary's response is what's served."""
        shadow_ids = self._shadows_for_fs.get(feature_set_ref, [])
        out: list[tuple[ModelMetadata, Model]] = []
        for mid in shadow_ids:
            entry = self._by_id.get(mid)
            if entry is not None:
                out.append(entry)
        return out

    def list_models(self) -> list[ModelMetadata]:
        """All registered metadata, ordered by model_id. Useful
        for health endpoints + audit."""
        return sorted((m for m, _ in self._by_id.values()), key=lambda m: m.model_id)

    def unregister(self, model_id: str) -> bool:
        """Remove a model. Returns True if it was present."""
        if model_id not in self._by_id:
            return False
        self._remove_from_role_indexes(model_id)
        del self._by_id[model_id]
        return True

    def _remove_from_role_indexes(self, model_id: str) -> None:
        # Scrub primary entries that point at this model_id.
        for fs, mid in list(self._primary_for_fs.items()):
            if mid == model_id:
                del self._primary_for_fs[fs]
        # Scrub shadow lists.
        for fs, shadows in list(self._shadows_for_fs.items()):
            if model_id in shadows:
                shadows.remove(model_id)
                if not shadows:
                    del self._shadows_for_fs[fs]
