"""Interactive (sync) inference path — PRED-08.

The gRPC server side of PRED-06's ``inference.v1.InferenceService``.
Companion to ``kanz_inference.streaming`` (async path); both invoke
the same ``Model`` protocol but with different concurrency models:

- streaming: one consumer goroutine drains the bus loop; back-
  pressure shows up as bus consumer lag.
- interactive: a bounded worker pool serves gRPC requests; back-
  pressure shows up as admission rejections (DEGRADED responses
  with degraded_reason = "pool_saturated").

The current entrypoint hosts both paths in one asyncio process with a shared
model router. Interactive admission is bounded; the streaming worker is a
separate task. Cross-path interference and latency budgets need measured
proof before an isolation claim (#1348).
"""

from kanz_inference.interactive.servicer import (
    DEFAULT_MAX_IN_FLIGHT,
    InferenceServicer,
    serve,
)

__all__ = ["DEFAULT_MAX_IN_FLIGHT", "InferenceServicer", "serve"]
