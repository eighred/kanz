"""Interactive (sync) inference path — PRED-08.

The gRPC server side of PRED-06's ``inference.v1.InferenceService``.
Companion to ``kanz_inference.streaming`` (async path); both invoke
the same ``Model`` protocol but with different concurrency models:

- streaming: one consumer goroutine drains the bus loop; back-
  pressure shows up as bus consumer lag.
- interactive: a bounded worker pool serves gRPC requests; back-
  pressure shows up as admission rejections (DEGRADED responses
  with degraded_reason = "pool_saturated").

The two paths are PROCESS-ISOLATED per KANZ_BRAIN — a burst of
interactive requests must not starve the streaming worker, and a
slow streaming workload must not delay interactive calls.
"""

from kanz_inference.interactive.servicer import (
    DEFAULT_MAX_IN_FLIGHT,
    InferenceServicer,
    serve,
)

__all__ = ["DEFAULT_MAX_IN_FLIGHT", "InferenceServicer", "serve"]
