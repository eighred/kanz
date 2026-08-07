"""The inference service's Prometheus series (#241).

WHY THIS FILE EXISTS. Until now this pod had no HTTP surface at all — one
containerPort (grpc 50051) and two tcpSocket probes against it — so every
degraded mode it implements was visible only in pod logs:

  * ADMISSION CONTROL rejecting Predict calls because ``in_flight`` reached
    ``max_in_flight`` (interactive/servicer.py). The servicer already fails fast
    and returns a DEGRADED envelope; nothing counted them, so the difference
    between "serving" and "refusing every second call" was a log line.
  * A MODEL THAT COULD NOT BE PROMOTED. The process exits 2 rather than serving
    without a validated PRIMARY (service.py), which is loud — but "which model is
    this pod actually serving" had no answer a dashboard could ask.
  * A STALLED DURABLE CONSUMER. The streaming worker shares one consumer group
    across replicas; a wedged subscription shows up as predictions simply not
    happening, which is indistinguishable from a quiet market unless the rate is
    a series.

``kanz_inference_*`` matches the Go estate's ``kanz_*`` convention, and the
scrape guard (kanz/test/arch/prometheus_scrape_test.go) plus the metric-declaration
guard (kanz/test/arch/observability_metrics_test.go) both read this file: a name
declared here counts as declared for any alert or SLO rule that names it.

THE DEFAULT REGISTRY IS DELIBERATE. prometheus_client's module-level REGISTRY
already carries the process/platform/GC collectors, which give RSS, open FDs and
CPU for free — the three things you want when the question is "why is
``in_flight`` pinned at the limit". A private registry would have to re-register
them by hand or lose them silently.
"""

from __future__ import annotations

from prometheus_client import Counter, Gauge, Histogram

from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

# The two code paths that produce a prediction. ONE label rather than two metric
# families, because every question worth asking ("what fraction of predictions
# are degraded") wants them summed, and a per-path family forces every rule to
# name both and go silently wrong when a third path arrives.
PATH_INTERACTIVE = "interactive"
PATH_STREAMING = "streaming"

MODE_NORMAL = "normal"
MODE_DEGRADED = "degraded"
# A cancelled call produced NO envelope — the client's deadline expired and it is
# no longer listening (servicer.py propagates CancelledError rather than
# synthesising a response). Counting it as degraded would attribute a client-side
# timeout to the model.
MODE_CANCELLED = "cancelled"

PREDICTIONS = Counter(
    "kanz_inference_predictions_total",
    "Predict calls that produced a PredictionEnvelope, by code path and envelope mode.",
    ["path", "mode"],
)

DEGRADED = Counter(
    "kanz_inference_degraded_total",
    "DEGRADED PredictionEnvelopes emitted, by code path and bounded degraded reason.",
    ["path", "reason"],
)

# NOT A SECOND COUNTER FOR ADMISSION REJECTS. An admission reject IS
# kanz_inference_degraded_total{path="interactive",reason="pool_saturated"} — the
# servicer returns a DEGRADED envelope for it, so a separate
# kanz_inference_admission_rejected_total would be two names for one event, free
# to disagree. These two gauges are what a reject counter could not tell you
# anyway: how close to the bound the pod is running BEFORE it starts refusing.
PREDICT_IN_FLIGHT = Gauge(
    "kanz_inference_predict_in_flight",
    "Predict calls currently executing on the interactive path.",
)

ADMISSION_LIMIT = Gauge(
    "kanz_inference_admission_limit",
    "Configured max_in_flight bound. in_flight reaching this is what produces a "
    "pool_saturated degraded response.",
)

# MEASURED ONLY WHERE THE MODEL ACTUALLY RAN. An admission reject returns in
# microseconds without touching the model; folding those into the same histogram
# drags every quantile down exactly when the service is most saturated, which is
# a latency panel that looks BEST during the incident.
PREDICT_SECONDS = Histogram(
    "kanz_inference_predict_seconds",
    "Wall time of a Model.predict call that was admitted and executed.",
    ["path"],
)

STREAM_OUTCOME_HANDLED = "handled"
STREAM_OUTCOME_UNMARSHAL_FAILED = "unmarshal_failed"
STREAM_OUTCOME_PUBLISH_FAILED = "publish_failed"

# WHAT THIS ADDS OVER kanz_inference_predictions_total{path="streaming"}: the two
# failure outcomes never produce a prediction at all (an undecodable payload is
# rejected before the model, a failed publish happens after it), so neither
# appears in that family. "handled" is the end-to-end completion — scored AND
# published — which is the rate a stalled-consumer alert has to watch.
STREAM_EVENTS = Counter(
    "kanz_inference_stream_events_total",
    "inference.feature.computed events the streaming worker took to completion, by outcome.",
    ["outcome"],
)

# 1 for the model card promoted to PRIMARY at boot. The promotion gate refuses a
# model whose validation is absent, failed or EXPIRED, so this answers "which
# weights is this replica serving" without reading a ConfigMap — and a rollout
# that half-promotes shows up as two model_id series at once.
PRIMARY_MODEL = Gauge(
    "kanz_inference_primary_model",
    "1 for each model card this process promoted to PRIMARY at startup.",
    ["model_id"],
)

REASON_UNSPECIFIED = "unspecified"
REASON_OTHER = "other"

# THE WIRE REASON IS FREE TEXT AND ONE OF THEM INTERPOLATES A WIRE FIELD.
# service.py builds ``f"{REASON_NO_PRIMARY}: {fv.feature_set_ref}"``, and
# feature_set_ref arrives on the bus from another service. Using degraded_reason
# directly as a label value is therefore one new time series per distinct
# feature_set_ref for the life of the process — the standard way a monitored
# service takes down the Prometheus monitoring it.
#
# So the wire string is MAPPED onto a fixed, small label set here and anything
# unrecognised collapses to "other". The keys cannot be imported from the modules
# that define them (streaming.worker and interactive.servicer both import THIS
# module, so the import would be circular), so they are literals — and
# tests/test_inference_metrics.py drives every one of those constants through
# reason_label and fails if any of them starts landing in "other".
_REASON_LABELS = {
    # interactive/servicer.py
    "pool_saturated": "pool_saturated",
    # interactive/servicer.py + streaming/worker.py
    "inference_unavailable": "inference_unavailable",
    # streaming/worker.py
    "no_cached_prediction": "no_cached_prediction",
    # service.py — carries ": <feature_set_ref>" after the prefix
    "no primary model for this feature set": "no_primary_model",
}


def reason_label(reason: str) -> str:
    """Map a wire ``degraded_reason`` onto a BOUNDED metric label value."""
    if not reason:
        return REASON_UNSPECIFIED
    return _REASON_LABELS.get(reason.split(":", 1)[0].strip(), REASON_OTHER)


def observe_prediction(path: str, envelope: PredictionEnvelope) -> PredictionEnvelope:
    """Count one produced envelope and RETURN IT, so a call site can wrap its return.

    Returning the argument is what makes this the single accounting point. Every
    ``return`` in the servicer and the worker goes through this call, so there is
    no path — including the ones that build a degraded envelope directly — that
    can produce an answer without counting it. The alternative (an ``.inc()``
    beside each return) is the shape where the next branch added silently emits
    an uncounted degraded response, which is precisely the state this whole
    change exists to end.
    """
    if envelope.mode == PredictionMode.PREDICTION_MODE_DEGRADED:
        PREDICTIONS.labels(path=path, mode=MODE_DEGRADED).inc()
        DEGRADED.labels(path=path, reason=reason_label(envelope.degraded_reason)).inc()
    else:
        PREDICTIONS.labels(path=path, mode=MODE_NORMAL).inc()
    return envelope
