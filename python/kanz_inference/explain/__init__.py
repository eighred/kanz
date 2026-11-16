"""Prediction explainability (MLOPS-01d).

Populates ``PredictionEnvelope.explanation`` (the SHAP-like per-feature
contribution map that has existed since PRED-01) on the inference path,
via a model-agnostic permutation-Shapley estimator — no heavy ``shap``
dependency.

Importable names:

- ``PermutationExplainer`` — Shapley values by permutation sampling;
  deterministic given a seed, contributions sum to
  ``pred(full) − pred(baseline)``.
- ``Explainer`` — the explainer Protocol.
- ``ExplainingModel`` — decorates a ``Model`` so its NORMAL predictions
  carry an explanation; drops into the StreamingWorker / interactive path.
"""

from kanz_inference.explain.explainer import (
    DEFAULT_SAMPLES,
    Explainer,
    ExplainingModel,
    PermutationExplainer,
)

__all__ = [
    "DEFAULT_SAMPLES",
    "Explainer",
    "ExplainingModel",
    "PermutationExplainer",
]
