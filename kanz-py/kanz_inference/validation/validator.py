"""Model validation suite (MLOPS-01b) — the evidence producer behind the
MLOPS-01a primary gate.

# What it does

Given a model and a labeled holdout/backtest dataset, it scores every sample,
computes the metrics a model-risk sign-off needs, decides pass/fail against
configurable thresholds, and emits a ``registry.ValidationRecord`` the registry
gate consumes. Three metric families, matching the board:

- **Holdout/backtest accuracy** — RMSE / MAE over (predicted value, realized
  actual). The core "is it any good" signal.
- **Calibration** — does the model's self-reported ``confidence`` track its
  realized accuracy? Measured as Expected Calibration Error (ECE). This is the
  piece that feeds PRED-02 §2.3: an **uncalibrated model gets a recommended
  ``confidence_threshold`` of 1.0 — every prediction DEGRADED** (the proto's
  "unknown confidence ⇒ no confidence" discipline). A calibrated model gets a
  threshold derived from the confidence level at which it actually meets the
  target accuracy.
- **Stability + bias** — bias is the mean signed error (systematic over/under-
  prediction); stability is the ratio of second-half to first-half RMSE over
  the (time-ordered) holdout, catching a model whose error degrades across the
  window (concept drift / instability) even when the aggregate looks fine.

# What it does NOT do

- Decide the validity *period* policy — the caller passes ``validity`` (default
  90 days); the suite only stamps ``expires_at = now + validity``.
- Persist or schedule. It returns a ``ValidationResult``; recording it
  (``Registry.record_validation``) and scheduling revalidation (MLOPS-01c) are
  separate steps.
"""

from __future__ import annotations

import math
from collections.abc import Sequence
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone

from inference.v1.feature_vector_pb2 import FeatureVector

from kanz_inference.registry import ValidationRecord
from kanz_inference.streaming.worker import Model


@dataclass(frozen=True)
class ValidationSample:
    """One labeled holdout point: the input the model scores plus the
    realized outcome. Samples are taken in time order, so the suite can
    assess stability across the window (first half vs second half)."""

    feature_vector: FeatureVector
    actual: float
    """The realized outcome the prediction is graded against."""


@dataclass(frozen=True)
class ValidationThresholds:
    """The pass/fail policy. Unset (``None``) accuracy bounds are not
    checked — useful when only calibration/stability matter."""

    min_samples: int = 30
    """Below this the holdout is too small to validate on — auto-fail
    (a sign-off on a handful of points is not a sign-off)."""

    max_rmse: float | None = None
    max_abs_bias: float | None = None

    max_calibration_error: float = 0.1
    """ECE above this ⇒ uncalibrated ⇒ confidence_threshold forced to 1.0."""

    max_stability_ratio: float = 2.0
    """second-half RMSE / first-half RMSE above this ⇒ unstable (error
    is degrading across the window)."""

    correct_tolerance: float = 0.5
    """A prediction counts as "correct" (for calibration/accuracy bucketing)
    when ``|value - actual| <= correct_tolerance``. The 0.5 default suits a
    probability/score output graded against a 0/1 label."""

    target_accuracy: float = 0.7
    """For a calibrated model, the recommended confidence_threshold is the
    lowest confidence at which accuracy reaches this."""

    calibration_bins: int = 10
    validity: timedelta = timedelta(days=90)


@dataclass(frozen=True)
class ValidationMetrics:
    n: int
    rmse: float
    mae: float
    bias: float
    calibration_error: float
    calibrated: bool
    stability_ratio: float


@dataclass(frozen=True)
class ValidationResult:
    """The suite's verdict on one model."""

    model_id: str
    passed: bool
    metrics: ValidationMetrics
    recommended_confidence_threshold: float
    reasons: tuple[str, ...] = field(default_factory=tuple)
    """Human-readable explanations for a failed (or noteworthy) result —
    e.g. ``"rmse 0.41 > max 0.30"``. Empty when cleanly passed."""

    def to_record(
        self,
        *,
        now: datetime | None = None,
        validity: timedelta = timedelta(days=90),
        report_uri: str = "",
    ) -> ValidationRecord:
        """Stamp this result as the MLOPS-01a ``ValidationRecord`` the
        registry primary gate consumes."""
        moment = now or datetime.now(timezone.utc)
        return ValidationRecord(
            model_id=self.model_id,
            validated_at=moment,
            expires_at=moment + validity,
            passed=self.passed,
            report_uri=report_uri,
        )


class Validator:
    """Runs the validation suite. Stateless apart from its thresholds —
    one instance validates many models."""

    def __init__(self, thresholds: ValidationThresholds | None = None) -> None:
        self.thresholds = thresholds or ValidationThresholds()

    async def validate(
        self, model: Model, samples: Sequence[ValidationSample]
    ) -> ValidationResult:
        """Score every sample, compute metrics, and decide pass/fail.

        Predictions are gathered in the order ``samples`` are given (so the
        stability split is the holdout's own time order). ``model.predict``
        is awaited per sample — a model that calls a remote backend stays
        non-blocking."""
        preds: list[float] = []
        confs: list[float] = []
        actuals: list[float] = []
        for s in samples:
            envelope = await model.predict(s.feature_vector)
            preds.append(envelope.value)
            confs.append(envelope.confidence)
            actuals.append(s.actual)

        return self._evaluate(model.model_id, preds, confs, actuals)

    # --- pure metric computation (sync, testable without a model) -------

    def _evaluate(
        self,
        model_id: str,
        preds: list[float],
        confs: list[float],
        actuals: list[float],
    ) -> ValidationResult:
        t = self.thresholds
        n = len(preds)
        reasons: list[str] = []

        if n < t.min_samples:
            metrics = ValidationMetrics(
                n=n, rmse=math.inf, mae=math.inf, bias=0.0,
                calibration_error=math.inf, calibrated=False, stability_ratio=math.inf,
            )
            reasons.append(f"only {n} samples (< min {t.min_samples})")
            return ValidationResult(
                model_id=model_id, passed=False, metrics=metrics,
                recommended_confidence_threshold=1.0, reasons=tuple(reasons),
            )

        errors = [p - a for p, a in zip(preds, actuals)]
        rmse = math.sqrt(sum(e * e for e in errors) / n)
        mae = sum(abs(e) for e in errors) / n
        bias = sum(errors) / n
        stability_ratio = _stability_ratio(errors)
        calibration_error, calibrated = _calibration(
            preds, confs, actuals, t.correct_tolerance, t.calibration_bins,
            t.max_calibration_error,
        )

        metrics = ValidationMetrics(
            n=n, rmse=rmse, mae=mae, bias=bias,
            calibration_error=calibration_error, calibrated=calibrated,
            stability_ratio=stability_ratio,
        )

        passed = True
        if t.max_rmse is not None and rmse > t.max_rmse:
            passed = False
            reasons.append(f"rmse {rmse:.4g} > max {t.max_rmse:.4g}")
        if t.max_abs_bias is not None and abs(bias) > t.max_abs_bias:
            passed = False
            reasons.append(f"|bias| {abs(bias):.4g} > max {t.max_abs_bias:.4g}")
        if stability_ratio > t.max_stability_ratio:
            passed = False
            reasons.append(
                f"stability ratio {stability_ratio:.4g} > max {t.max_stability_ratio:.4g}"
            )
        if not calibrated:
            # Uncalibrated is not a hard fail of the *model* — it is allowed to
            # serve, but every prediction is DEGRADED (threshold 1.0). It is the
            # accuracy/stability bounds that gate primary-ness.
            reasons.append(
                f"uncalibrated (ECE {calibration_error:.4g} > {t.max_calibration_error:.4g}) "
                "⇒ confidence_threshold forced to 1.0"
            )

        threshold = _recommend_threshold(
            preds, confs, actuals, t.correct_tolerance, t.target_accuracy, calibrated
        )

        return ValidationResult(
            model_id=model_id, passed=passed, metrics=metrics,
            recommended_confidence_threshold=threshold, reasons=tuple(reasons),
        )


def _is_correct(pred: float, actual: float, tolerance: float) -> bool:
    return abs(pred - actual) <= tolerance


def _stability_ratio(errors: list[float]) -> float:
    """second-half RMSE / first-half RMSE over the time-ordered errors. 1.0
    means equally accurate across the window; >1 means error grew. A zero
    first-half RMSE floors to a tiny epsilon so a degrading model still
    surfaces as a large ratio rather than a divide-by-zero."""
    n = len(errors)
    mid = n // 2
    first, second = errors[:mid], errors[mid:]
    if not first or not second:
        return 1.0
    rmse_first = math.sqrt(sum(e * e for e in first) / len(first))
    rmse_second = math.sqrt(sum(e * e for e in second) / len(second))
    return rmse_second / max(rmse_first, 1e-12)


def _calibration(
    preds: list[float],
    confs: list[float],
    actuals: list[float],
    tolerance: float,
    bins: int,
    max_ece: float,
) -> tuple[float, bool]:
    """Expected Calibration Error of the self-reported confidence against
    realized correctness, plus whether the model is deemed calibrated.

    A model that reports no confidence (all zero — the proto's "no calibrated
    confidence" sentinel) is uncalibrated by definition, regardless of ECE."""
    if max(confs, default=0.0) <= 0.0:
        return math.inf, False  # no confidence signal at all ⇒ uncalibrated

    n = len(preds)
    correct = [1.0 if _is_correct(p, a, tolerance) else 0.0 for p, a in zip(preds, actuals)]
    # Bin by confidence in [0, 1]; each prediction's bin contributes its gap
    # between mean confidence and empirical accuracy, weighted by bin size.
    bin_conf = [0.0] * bins
    bin_acc = [0.0] * bins
    bin_count = [0] * bins
    for c, ok in zip(confs, correct):
        idx = min(int(c * bins), bins - 1) if c < 1.0 else bins - 1
        bin_conf[idx] += c
        bin_acc[idx] += ok
        bin_count[idx] += 1

    ece = 0.0
    for b in range(bins):
        if bin_count[b] == 0:
            continue
        mean_conf = bin_conf[b] / bin_count[b]
        acc = bin_acc[b] / bin_count[b]
        ece += (bin_count[b] / n) * abs(mean_conf - acc)

    return ece, ece <= max_ece


def _recommend_threshold(
    preds: list[float],
    confs: list[float],
    actuals: list[float],
    tolerance: float,
    target_accuracy: float,
    calibrated: bool,
) -> float:
    """For a calibrated model, the lowest confidence ``c`` such that
    predictions with ``confidence >= c`` reach ``target_accuracy`` — the
    PRED-02 §2.3 threshold below which predictions are DEGRADED. Uncalibrated
    ⇒ 1.0 (everything DEGRADED). If no threshold meets the target ⇒ 1.0."""
    if not calibrated:
        return 1.0
    candidates = sorted({c for c in confs if c > 0.0})
    for c in candidates:
        kept = [
            _is_correct(p, a, tolerance)
            for p, conf, a in zip(preds, confs, actuals)
            if conf >= c
        ]
        if kept and (sum(kept) / len(kept)) >= target_accuracy:
            return c
    return 1.0
