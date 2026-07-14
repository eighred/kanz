"""Composition root for the inference service (AI-M1).

The parts all existed. This is the file that makes them a SERVICE: it loads model
cards, drives them through the registry's promotion gate, routes an inbound
FeatureVector to the model that owns its feature set, and refuses to come up with
nothing to serve.

# Two postures, and they are opposites on purpose

AT STARTUP it FAILS CLOSED. A process with no PRIMARY model does not start. This
is the same rule the rest of the platform already applies — the gateway refuses
to start unauthenticated (SEC-M1), webhook-ingest refuses to start without a
replay defence (EXEC-M17/M22), tv-sync refuses to start without a fact log
(EXEC-M21). A prediction service that serves nothing is worse than one that is
down, because it reports healthy and it answers.

AT REQUEST TIME it FAILS SOFT. A feature set with no primary comes back as a
DEGRADED envelope with zero confidence — never an exception, never a silent zero.
"I could not predict" is an answer a consumer can act on; a black hole is not.
The Go client (internal/prediction/sync_client.go) is built around exactly this
contract: circuit breaker, last-known cache, degraded fallback.
"""

from __future__ import annotations

import json
import logging
from datetime import datetime
from pathlib import Path
from typing import Iterable, Sequence

from google.protobuf.timestamp_pb2 import Timestamp

from inference.v1.feature_vector_pb2 import FeatureValue, FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.explain.explainer import PermutationExplainer
from kanz_inference.models.linear import LinearModel, ModelSpec
from kanz_inference.registry.registry import Registry

logger = logging.getLogger(__name__)

__all__ = [
    "NoPrimaryModelError",
    "RegistryRouter",
    "build_registry",
    "load_specs",
    "require_primary",
]

REASON_NO_PRIMARY = "no primary model for this feature set"


class NoPrimaryModelError(RuntimeError):
    """Raised at STARTUP when nothing is registered to serve.

    Deliberately fatal. The alternative — booting and answering every request
    with a degraded envelope — is a service that is wrong in a way its own health
    check reports as fine.
    """


def build_registry(specs: Iterable[ModelSpec], *, clock=None) -> Registry:
    """Register each spec as PRIMARY for its feature set.

    The promotion gate does the work: ``register(..., primary=True)`` raises
    unless a passing, non-expired ValidationRecord is already on file. So a model
    whose validation has LAPSED cannot be promoted — model risk is a revalidation
    cadence, not a one-time sign-off — and there is no code path here that
    registers weights without the evidence.
    """
    registry = Registry(clock=clock)
    for spec in specs:
        record = spec.validation()
        if record is not None:
            registry.record_validation(record)
        # No record, a failed one, or an expired one ⇒ this raises. That is the
        # gate, and it is the whole point of it.
        registry.register(spec.metadata(), LinearModel(spec), primary=True)
        logger.info(
            "model registered as PRIMARY: %s (feature_set=%s, threshold=%.3f)",
            spec.model_id,
            spec.feature_set_ref,
            spec.confidence_threshold,
        )
    return registry


def require_primary(registry: Registry) -> None:
    """Refuse to serve nothing. Called at startup, before the port is bound."""
    if not registry.list_models():
        raise NoPrimaryModelError(
            "no model is registered to serve — refusing to start. A prediction "
            "service with no model reports healthy and answers anyway, which is "
            "worse than being down. Set KANZ_INFERENCE_MODELS to a model-card file."
        )


def load_specs(path: str | Path) -> list[ModelSpec]:
    """Load model cards from a JSON file (a mounted ConfigMap in the cluster).

    The weights and the validation evidence live in the same document, because
    the registry will not accept one without the other.
    """
    raw = json.loads(Path(path).read_text(encoding="utf-8"))
    cards = raw if isinstance(raw, list) else [raw]
    return [_spec_from(card) for card in cards]


def _spec_from(card: dict) -> ModelSpec:
    def when(key: str) -> datetime | None:
        v = card.get(key)
        return datetime.fromisoformat(v) if v else None

    return ModelSpec(
        model_id=card["model_id"],
        feature_set_ref=card["feature_set_ref"],
        confidence_threshold=float(card["confidence_threshold"]),
        weights={k: float(v) for k, v in card["weights"].items()},
        bias=float(card.get("bias", 0.0)),
        validated_at=when("validated_at"),
        expires_at=when("expires_at"),
        validation_passed=bool(card.get("validation_passed", True)),
        artifact_uri=card.get("artifact_uri", ""),
        report_uri=card.get("report_uri", ""),
        scale=float(card.get("scale", 1.0)),
    )


class RegistryRouter:
    """A ``Model`` that routes each FeatureVector to the PRIMARY for its feature set.

    It satisfies the same Protocol the servicer and the streaming worker consume,
    so one object serves BOTH paths (interactive gRPC + streaming bus) and there
    is exactly one place where routing and degradation are decided.

    A model is found by ``feature_set_ref``, never by name — so a model can never
    be handed a vector it was not trained on, which is the failure a routing table
    keyed on anything else eventually produces.
    """

    model_id = "registry-router"

    def __init__(self, registry: Registry, *, explain: bool = False, explainer=None) -> None:
        if registry is None:
            raise ValueError("registry required")
        self._registry = registry
        self._explain = explain
        self._explainer = explainer  # None ⇒ one is built per model (see _explainer_for)
        self._per_model: dict[str, PermutationExplainer] = {}

    def _explainer_for(self, model) -> PermutationExplainer:
        """An explainer whose baseline is ZERO for every feature the model weighs.

        THE DEFAULT BASELINE DROPS THE FEATURE, and that is incompatible with a
        model that REFUSES to score a partial vector (LinearModel degrades when a
        trained-on feature is absent — a missing feature is a hole in the input,
        not a zero). Ablate by dropping and every coalition below the full set
        comes back DEGRADED with value 0, which silently turns the attribution
        into "every feature contributed value/n" — a plausible-looking number
        that means nothing.

        For a linear model "absent" means "contributes nothing", i.e. x = 0 — not
        "the field does not exist". So the reference value is 0.0, the model
        always sees a complete vector, and w·0 = 0 gives the baseline the Shapley
        decomposition actually wants.
        """
        if self._explainer is not None:
            return self._explainer
        cached = self._per_model.get(model.model_id)
        if cached is not None:
            return cached
        baseline = {}
        spec = getattr(model, "spec", None)
        if spec is not None:
            baseline = {name: FeatureValue(scalar=0.0) for name in spec.weights}
        built = PermutationExplainer(baseline=baseline)
        self._per_model[model.model_id] = built
        return built

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        entry = self._registry.primary_for_feature_set(fv.feature_set_ref)
        if entry is None:
            # FAIL SOFT. The caller gets an answer that says "I don't know",
            # which is something it can act on. An exception here would become a
            # black hole in the Go client's breaker, and a zero would become a
            # confident-looking lie.
            as_of = Timestamp()
            as_of.CopyFrom(fv.as_of)
            logger.warning("no primary model for feature_set=%s", fv.feature_set_ref)
            return PredictionEnvelope(
                subject_id=fv.subject_id,
                model=self.model_id,
                value=0.0,
                confidence=0.0,  # unknown confidence IS no confidence
                mode=PredictionMode.PREDICTION_MODE_DEGRADED,
                degraded_reason=f"{REASON_NO_PRIMARY}: {fv.feature_set_ref}",
                as_of=as_of,
            )

        _, model = entry
        prediction = await model.predict(fv)

        if self._explain and prediction.mode == PredictionMode.PREDICTION_MODE_NORMAL:
            # Best-effort: a prediction that cannot be explained is still a
            # prediction, but one the risk desk cannot audit. Never let the
            # explanation failure take the prediction down with it.
            try:
                contributions = await self._explainer_for(model).explain(model, fv)
                for name, contribution in contributions.items():
                    prediction.explanation[name] = contribution
            except Exception as e:  # noqa: BLE001
                logger.warning("explanation failed for %s: %s", model.model_id, e)

        return prediction

    def feature_sets(self) -> Sequence[str]:
        return [m.feature_set_ref for m in self._registry.list_models()]
