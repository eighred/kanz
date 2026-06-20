"""Champion/challenger promotion (MLOPS-01e).

Turns PRED-10's shadow evaluation into an automated, gated promotion
decision. The PRED-10 ``ShadowExecutor`` already runs challengers
(shadows) in parallel with the champion (primary) and hands every
(primary, shadow) prediction pair to a ``ShadowObserver``.
``ShadowComparisonStore`` is that observer: it accumulates the
comparison metrics a promotion needs. ``Promoter`` reads the store and
promotes a challenger only when three independent gates all hold.

# Why out-performance needs an outcome join

A prediction pair alone cannot say which model is *better* — that needs
ground truth. ``observe`` (the live path) only sees the two predictions,
so the store also exposes ``record_outcome(subject_id, actual)``: when
the realized outcome for a subject arrives (a later consumer joins it),
the store credits each model's squared error. Live RMSE — real
out-performance on production traffic — falls out of that. Behavioral
stats that need no labels (divergence, disagreement rate, observation
count) are accumulated on ``observe`` directly.

# The three promotion gates (all required)

1. **Soak** — the challenger has handled enough live comparisons
   (``min_observations``) and enough have been scored against outcomes
   (``min_scored``). Don't promote on a handful of requests.
2. **Validation** — the challenger has a recorded, non-expired, passing
   MLOPS-01a ``ValidationRecord`` (and ``Registry.register(primary=True)``
   re-checks it on promotion — belt and suspenders).
3. **Out-performance** — the challenger's live RMSE beats the champion's
   by at least ``improvement_margin`` (a tie or marginal win does not
   justify the switch risk).

Promotion re-registers the winner as primary and demotes the old
champion to shadow, so it keeps running for comparison.
"""

from __future__ import annotations

import logging
import math
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Callable

from inference.v1.feature_vector_pb2 import FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope

from kanz_inference.registry import ModelMetadata, Registry

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class ComparisonStats:
    """Read view of one (champion, challenger) pair's accumulated metrics."""

    primary_id: str
    challenger_id: str
    observations: int
    scored: int
    mean_abs_divergence: float
    disagreement_rate: float
    primary_rmse: float | None
    challenger_rmse: float | None


@dataclass
class _Acc:
    observations: int = 0
    abs_divergence_sum: float = 0.0
    disagreements: int = 0
    scored: int = 0
    primary_sq_err: float = 0.0
    challenger_sq_err: float = 0.0


class ShadowComparisonStore:
    """A ``ShadowObserver`` (structural — matches the PRED-10 protocol) that
    accumulates champion/challenger comparison metrics keyed by
    ``(primary_id, challenger_id)``.

    Behavioral metrics (divergence, disagreement, count) accrue on
    ``observe``; accuracy (RMSE) accrues when ``record_outcome`` joins a
    realized value for a previously-observed subject.
    """

    def __init__(self) -> None:
        self._acc: dict[tuple[str, str], _Acc] = {}
        # subject_id → {pair_key: (primary_value, challenger_value)} awaiting
        # an outcome. Latest prediction per (subject, pair) wins.
        self._pending: dict[str, dict[tuple[str, str], tuple[float, float]]] = {}

    async def observe(
        self,
        feature_vector: FeatureVector,
        primary: ModelMetadata,
        primary_prediction: PredictionEnvelope,
        shadow: ModelMetadata,
        shadow_prediction: PredictionEnvelope,
    ) -> None:
        key = (primary.model_id, shadow.model_id)
        acc = self._acc.setdefault(key, _Acc())
        pv, sv = primary_prediction.value, shadow_prediction.value
        acc.observations += 1
        acc.abs_divergence_sum += abs(sv - pv)
        if pv * sv < 0:  # opposite directional sign
            acc.disagreements += 1
        self._pending.setdefault(feature_vector.subject_id, {})[key] = (pv, sv)

    def record_outcome(self, subject_id: str, actual: float) -> None:
        """Join a realized outcome for a subject, crediting squared error to
        both models of every pair that has a pending prediction for it. A
        no-op for an unknown subject (the outcome arrived for something never
        shadow-compared)."""
        pending = self._pending.pop(subject_id, None)
        if not pending:
            return
        for key, (pv, sv) in pending.items():
            acc = self._acc.setdefault(key, _Acc())
            acc.scored += 1
            acc.primary_sq_err += (pv - actual) ** 2
            acc.challenger_sq_err += (sv - actual) ** 2

    def stats(self, primary_id: str, challenger_id: str) -> ComparisonStats:
        """The accumulated comparison for one pair (zero-valued when the pair
        has never been observed)."""
        acc = self._acc.get((primary_id, challenger_id), _Acc())
        n = acc.observations
        return ComparisonStats(
            primary_id=primary_id,
            challenger_id=challenger_id,
            observations=n,
            scored=acc.scored,
            mean_abs_divergence=(acc.abs_divergence_sum / n) if n else 0.0,
            disagreement_rate=(acc.disagreements / n) if n else 0.0,
            primary_rmse=_rmse(acc.primary_sq_err, acc.scored),
            challenger_rmse=_rmse(acc.challenger_sq_err, acc.scored),
        )


@dataclass(frozen=True)
class PromotionDecision:
    """The outcome of one promotion evaluation for a feature set."""

    feature_set_ref: str
    promoted: str | None
    """The challenger model_id promoted to primary, or None if none qualified."""
    previous_primary: str | None
    reasons: tuple[str, ...] = field(default_factory=tuple)
    """Per-candidate explanation (why each challenger was or wasn't promoted)."""


class Promoter:
    """Evaluates challengers for one or more feature sets and promotes the
    best qualifying one. Stateless apart from config + collaborators."""

    def __init__(
        self,
        registry: Registry,
        store: ShadowComparisonStore,
        *,
        min_observations: int = 100,
        min_scored: int = 30,
        improvement_margin: float = 0.05,
        clock: Callable[[], datetime] | None = None,
    ) -> None:
        if registry is None:
            raise ValueError("registry required")
        if store is None:
            raise ValueError("store required")
        if not 0.0 <= improvement_margin < 1.0:
            raise ValueError("improvement_margin must be in [0, 1)")
        self._registry = registry
        self._store = store
        self._min_observations = min_observations
        self._min_scored = min_scored
        self._margin = improvement_margin
        self._clock = clock or (lambda: datetime.now(timezone.utc))

    def evaluate(self, feature_set_ref: str) -> PromotionDecision:
        """Assess every challenger for ``feature_set_ref`` and, if one passes
        all three gates and out-performs the field, promote it. Pure decision
        + a registry mutation on promotion; safe to call on a schedule."""
        primary = self._registry.primary_for_feature_set(feature_set_ref)
        if primary is None:
            return PromotionDecision(
                feature_set_ref, None, None, ("no primary registered",)
            )
        champion_meta, champion_model = primary
        champion_id = champion_meta.model_id
        now = self._clock()

        reasons: list[str] = []
        best: tuple[ModelMetadata, object, float] | None = None  # (meta, model, challenger_rmse)
        for challenger_meta, challenger_model in self._registry.shadows_for_feature_set(
            feature_set_ref
        ):
            cid = challenger_meta.model_id
            stats = self._store.stats(champion_id, cid)

            if stats.observations < self._min_observations:
                reasons.append(f"{cid}: soak {stats.observations} < {self._min_observations} observations")
                continue
            if stats.scored < self._min_scored:
                reasons.append(f"{cid}: only {stats.scored} < {self._min_scored} scored outcomes")
                continue
            record = self._registry.validation_for(cid)
            if record is None or not record.is_valid(now):
                reasons.append(f"{cid}: no current passing validation")
                continue
            if stats.challenger_rmse is None or stats.primary_rmse is None:
                reasons.append(f"{cid}: rmse unavailable")
                continue
            if stats.challenger_rmse > stats.primary_rmse * (1.0 - self._margin):
                reasons.append(
                    f"{cid}: rmse {stats.challenger_rmse:.4g} does not beat "
                    f"champion {stats.primary_rmse:.4g} by {self._margin:.0%}"
                )
                continue

            reasons.append(
                f"{cid}: QUALIFIES (rmse {stats.challenger_rmse:.4g} vs "
                f"champion {stats.primary_rmse:.4g})"
            )
            if best is None or stats.challenger_rmse < best[2]:
                best = (challenger_meta, challenger_model, stats.challenger_rmse)

        if best is None:
            return PromotionDecision(feature_set_ref, None, champion_id, tuple(reasons))

        winner_meta, winner_model, _ = best
        # Promote winner to primary (re-checks the MLOPS-01a gate), then demote
        # the old champion to shadow so it keeps running for comparison.
        self._registry.register(winner_meta, winner_model, primary=True)
        self._registry.register(champion_meta, champion_model, primary=False)
        logger.info(
            "promoted %s over %s for %s", winner_meta.model_id, champion_id, feature_set_ref
        )
        return PromotionDecision(
            feature_set_ref, winner_meta.model_id, champion_id, tuple(reasons)
        )


def _rmse(sq_err_sum: float, n: int) -> float | None:
    return math.sqrt(sq_err_sum / n) if n else None
