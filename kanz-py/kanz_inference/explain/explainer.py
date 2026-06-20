"""Prediction explainability (MLOPS-01d) — populate
``PredictionEnvelope.explanation`` with per-feature contributions.

# Model-agnostic, dependency-free

The contributions are Shapley values estimated by **permutation sampling**
(Štrumbelj–Kononenko): for each of several random feature orderings, add
the features one at a time to a baseline coalition and credit each feature
with the change in the model's output when it joins. Averaged over
orderings this approximates the Shapley value — the same attribution
``shap`` computes — but model-agnostic (only needs ``model.predict``) and
without pulling the heavy ``shap`` dependency into the inference image.

Two properties hold *exactly*, regardless of the sample count, because
each ordering's per-feature credits telescope:

- **Efficiency** — the contributions sum to ``pred(full) − pred(baseline)``
  (matching the proto's "sum need not equal ``value``": it equals the
  value *minus the baseline prediction*, the honest attribution total).
- **Determinism** — a fixed ``seed`` + the stable feature ordering make
  the explanation reproducible across replays (EVT-21d), so an audit
  reconstructs the same attribution.

# The baseline

A feature's contribution is measured against a baseline ("what if this
feature were absent?"). The default baseline *drops* the feature from the
vector — the proto's "omit the entry ⇒ feature absent" semantics. A
caller whose model refuses partial input can instead supply reference
values per feature (``baseline=``); features without a reference are
dropped.

# Wiring it onto the inference path

``ExplainingModel`` decorates any ``Model``: it returns the inner
prediction with ``explanation`` filled in. Wrap the worker's / servicer's
model with it to turn on explanations without touching the model itself.
Only NORMAL predictions are explained — attributing a degraded fallback
(stale cache, zero-valued) would be misleading. Explanation is best-effort:
if attribution fails (e.g. the model rejects a perturbed vector) the
prediction is returned unexplained rather than failing the inference.
"""

from __future__ import annotations

import logging
import random
from typing import Mapping, Protocol, runtime_checkable

from inference.v1.feature_vector_pb2 import FeatureValue, FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.streaming.worker import Model

logger = logging.getLogger(__name__)

# Default number of permutations sampled. Each adds at most one model call
# per distinct coalition (memoized within a single explain), so this trades
# attribution variance against cost. Linear/additive models converge at 1.
DEFAULT_SAMPLES = 32


@runtime_checkable
class Explainer(Protocol):
    """Computes per-feature contributions for one prediction. Async because
    it drives ``model.predict`` on perturbed inputs."""

    async def explain(self, model: Model, fv: FeatureVector) -> dict[str, float]:
        ...


class PermutationExplainer:
    """Shapley-value explainer via permutation sampling. Stateless across
    calls apart from its config — one instance explains many models."""

    def __init__(
        self,
        *,
        samples: int = DEFAULT_SAMPLES,
        seed: int = 0,
        baseline: Mapping[str, FeatureValue] | None = None,
    ) -> None:
        if samples < 1:
            raise ValueError("samples must be >= 1")
        self._samples = samples
        self._seed = seed
        self._baseline = dict(baseline) if baseline else {}

    async def explain(self, model: Model, fv: FeatureVector) -> dict[str, float]:
        features = sorted(fv.values.keys())  # stable order ⇒ deterministic
        n = len(features)
        if n == 0:
            return {}

        cache: dict[frozenset[str], float] = {}

        async def pred_value(present: frozenset[str]) -> float:
            if present in cache:
                return cache[present]
            envelope = await model.predict(self._coalition(fv, present))
            cache[present] = envelope.value
            return envelope.value

        contributions = {f: 0.0 for f in features}
        rng = random.Random(self._seed)
        for _ in range(self._samples):
            order = features[:]
            rng.shuffle(order)
            present: set[str] = set()
            prev = await pred_value(frozenset(present))
            for f in order:
                present.add(f)
                cur = await pred_value(frozenset(present))
                contributions[f] += cur - prev
                prev = cur

        return {f: contributions[f] / self._samples for f in features}

    def _coalition(self, fv: FeatureVector, present: frozenset[str]) -> FeatureVector:
        """A copy of ``fv`` where present features keep their real value,
        absent features take their baseline reference (or are dropped)."""
        out = FeatureVector(subject_id=fv.subject_id, feature_set_ref=fv.feature_set_ref)
        out.as_of.CopyFrom(fv.as_of)
        out.source_event_ids.extend(fv.source_event_ids)
        for name, fval in fv.values.items():
            if name in present:
                out.values[name].CopyFrom(fval)
            elif name in self._baseline:
                out.values[name].CopyFrom(self._baseline[name])
            # otherwise the feature is absent (dropped)
        return out


class ExplainingModel:
    """Decorates a ``Model`` so its NORMAL predictions carry an
    ``explanation``. Satisfies the ``Model`` protocol, so it drops into the
    StreamingWorker / interactive path in place of the bare model."""

    def __init__(self, inner: Model, explainer: Explainer, *, only_normal: bool = True) -> None:
        if inner is None:
            raise ValueError("inner model required")
        if explainer is None:
            raise ValueError("explainer required")
        self._inner = inner
        self._explainer = explainer
        self._only_normal = only_normal
        self.model_id = inner.model_id

    async def predict(self, fv: FeatureVector) -> PredictionEnvelope:
        prediction = await self._inner.predict(fv)
        if self._only_normal and prediction.mode != PredictionMode.PREDICTION_MODE_NORMAL:
            return prediction
        try:
            contributions = await self._explainer.explain(self._inner, fv)
        except Exception as e:  # noqa: BLE001 — explanation is best-effort
            logger.warning(
                "explanation failed for %s (%s) — returning prediction unexplained",
                self.model_id,
                e,
            )
            return prediction
        for name, contribution in contributions.items():
            prediction.explanation[name] = contribution
        return prediction
