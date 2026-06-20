"""Model validation suite (MLOPS-01b).

Produces the validation evidence the MLOPS-01a registry primary gate
requires: holdout/backtest accuracy (RMSE/MAE), a calibration check
(ECE) that drives the PRED-02 §2.3 per-model ``confidence_threshold``
(uncalibrated ⇒ 1.0 ⇒ every prediction DEGRADED), and stability/bias.

Importable names:

- ``Validator`` — runs the suite over a model + labeled holdout.
- ``ValidationSample`` — one labeled holdout point (feature vector + actual).
- ``ValidationThresholds`` — the pass/fail policy.
- ``ValidationMetrics`` — the computed metrics.
- ``ValidationResult`` — the verdict; ``.to_record(...)`` stamps the
  MLOPS-01a ``ValidationRecord``.
"""

from kanz_inference.validation.validator import (
    ValidationMetrics,
    ValidationResult,
    ValidationSample,
    ValidationThresholds,
    Validator,
)

__all__ = [
    "ValidationMetrics",
    "ValidationResult",
    "ValidationSample",
    "ValidationThresholds",
    "Validator",
]
