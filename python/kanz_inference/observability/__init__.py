"""Observability for the inference service — #241.

The Prometheus series this pod exports (``metrics``) and the HTTP surface that
serves them alongside the two probe routes (``server``). Before this the pod
opened one port, grpc 50051, and was structurally unscrapable: every degraded
mode it implements — admission rejections, an unpromotable model, a stalled
durable consumer — existed only as a log line.
"""

from kanz_inference.observability import metrics
from kanz_inference.observability.server import (
    DEFAULT_METRICS_BIND,
    LIVE_PATH,
    METRICS_PATH,
    READY_PATH,
    ObservabilityServer,
)

__all__ = [
    "DEFAULT_METRICS_BIND",
    "LIVE_PATH",
    "METRICS_PATH",
    "READY_PATH",
    "ObservabilityServer",
    "metrics",
]
