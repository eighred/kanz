"""Drift → revalidation loop (MLOPS-01c).

Closes the governance loop DATA-04 (drift detection) and MLOPS-01a/b
(validation) left open: when a feature a model depends on drifts, the
model's recorded validation is no longer trustworthy, so the model must
be revalidated (and possibly retrained). This consumes the DATA-07
``data.feature.drift_detected`` event and emits an automated
revalidation/retrain signal for every affected model.

# The two boundaries

A drift event names a single drifted ``feature`` (observation.v1
DriftDetail), but the registry indexes models by ``feature_set_ref``, not
by individual feature — there is no feature→model index in the registry.
So resolution is a pluggable ``ModelResolver`` (the deployment's feature
catalog knows which models consume a feature), mirroring the MODEL-01f
``factor.Classifier`` stand-in pattern. The signal goes out through a
``RevalidationSink`` (publish a command on the bus, enqueue a retrain
job, page an MLOps queue), mirroring PRED-05's ``PredictionPublisher``
— this package owns the *trigger logic*, not the delivery.

# Why a cooldown

A drift detector re-emits on every window the feature stays drifted, so a
persistent drift would fire a revalidation storm. A per-(model, feature)
cooldown collapses the burst into one signal per window — same intent as
the recompute debounce on the Go side.
"""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Protocol, runtime_checkable

from envelope.v1.envelope_pb2 import Envelope
from observation.v1.data_quality_pb2 import DataQualityEvent, Severity

from kanz_bus import Subscriber

logger = logging.getLogger(__name__)


# Subject the DATA-04 detector publishes feature drift on
# (kanz-schemas/README.md § Subject Taxonomy §1).
SUBJECT_DRIFT_DETECTED = "data.feature.drift_detected"


@dataclass(frozen=True)
class RevalidationRequest:
    """The automated revalidation/retrain signal emitted for one affected
    model. Carries the drift evidence so the downstream consumer (a
    validation job, a retrain pipeline, an MLOps ticket) has the full
    triage context without re-reading the source event."""

    model_id: str
    feature: str
    metric: str
    score: float
    threshold: float
    severity: int  # observation.v1.Severity
    subject: str
    partition_key: str
    baseline_ref: str
    reason: str
    requested_at: datetime


@runtime_checkable
class ModelResolver(Protocol):
    """Maps a drifted feature to the model_ids that depend on it. The
    deployment's feature catalog implements this; the registry cannot
    (it indexes by feature_set_ref, not by feature)."""

    def affected_models(self, feature: str) -> list[str]:
        ...


class StaticModelResolver:
    """In-memory ``ModelResolver`` backed by a fixed feature→model_ids
    map — the stand-in the service loads from the feature catalog (the
    mirror of MODEL-01f's StaticClassifier). Tests construct it directly."""

    def __init__(self, feature_to_models: dict[str, list[str]]) -> None:
        self._map = {f: list(ids) for f, ids in feature_to_models.items()}

    def affected_models(self, feature: str) -> list[str]:
        return list(self._map.get(feature, []))


@runtime_checkable
class RevalidationSink(Protocol):
    """Where a revalidation/retrain signal goes. Implementations publish a
    command on the bus, enqueue a retrain job, or open an MLOps ticket —
    this package does not choose the delivery."""

    async def request_revalidation(self, request: RevalidationRequest) -> None:
        ...


class LoggingRevalidationSink:
    """Default sink — logs the request. Useful as an observability-only
    deployment and in tests (mirrors shadow.LoggingShadowObserver)."""

    async def request_revalidation(self, request: RevalidationRequest) -> None:
        logger.info(
            "revalidation requested: model=%s feature=%s %s=%.4g>%.4g",
            request.model_id,
            request.feature,
            request.metric,
            request.score,
            request.threshold,
        )


_SEVERITY_NAME = {
    Severity.SEVERITY_WARNING: "WARNING",
    Severity.SEVERITY_CRITICAL: "CRITICAL",
}


class DriftTrigger:
    """Consumes ``data.feature.drift_detected`` and fans out a
    RevalidationRequest per affected model.

    Construct once; call ``run`` to subscribe, or drive ``handle``
    directly (public, like StreamingWorker.handle, so tests need no
    subscriber).
    """

    def __init__(
        self,
        resolver: ModelResolver,
        sink: RevalidationSink,
        *,
        min_severity: int = Severity.SEVERITY_WARNING,
        cooldown: timedelta = timedelta(hours=1),
        clock: Callable[[], datetime] | None = None,
    ) -> None:
        if resolver is None:
            raise ValueError("resolver required")
        if sink is None:
            raise ValueError("sink required")
        self._resolver = resolver
        self._sink = sink
        self._min_severity = min_severity
        self._cooldown = cooldown
        self._clock = clock or (lambda: datetime.now(timezone.utc))
        # Per-(model_id, feature) last-fired time for cooldown suppression.
        self._last_fired: dict[tuple[str, str], datetime] = {}

    async def run(
        self, subscriber: Subscriber, group: str, subject: str = SUBJECT_DRIFT_DETECTED
    ) -> None:
        """Subscribe to the drift subject under ``group`` and process
        events until cancellation."""
        await subscriber.subscribe(subject, group, self.handle)

    async def handle(self, env: Envelope, payload: bytes) -> None:
        """One inbound data-quality event. Acts only on the drift variant;
        gap/staleness (or a sub-threshold severity) are ignored no-ops so a
        broad subscription does not error on them."""
        event = DataQualityEvent()
        try:
            event.ParseFromString(payload)
        except Exception as e:  # noqa: BLE001 — re-raised after logging (DLQ path)
            logger.error("drift payload unmarshal failed: %s", e)
            raise

        if event.WhichOneof("detail") != "drift":
            return  # not a drift event — nothing to do
        if event.severity < self._min_severity:
            return  # below the action threshold

        drift = event.drift
        feature = drift.feature
        if not feature:
            logger.warning("drift event with empty feature on %s — skipping", event.subject)
            return

        models = self._resolver.affected_models(feature)
        if not models:
            logger.info("drift on feature %r affects no registered model", feature)
            return

        now = self._clock()
        sev = _SEVERITY_NAME.get(event.severity, str(event.severity))
        for model_id in models:
            key = (model_id, feature)
            last = self._last_fired.get(key)
            if last is not None and now - last < self._cooldown:
                logger.debug(
                    "revalidation for %s/%s within cooldown — suppressed", model_id, feature
                )
                continue
            self._last_fired[key] = now
            request = RevalidationRequest(
                model_id=model_id,
                feature=feature,
                metric=drift.metric,
                score=drift.score,
                threshold=drift.threshold,
                severity=event.severity,
                subject=event.subject,
                partition_key=event.partition_key,
                baseline_ref=drift.baseline_ref,
                reason=(
                    f"drift on feature {feature!r}: {drift.metric} "
                    f"{drift.score:.4g} > {drift.threshold:.4g} (severity {sev})"
                ),
                requested_at=now,
            )
            await self._sink.request_revalidation(request)
