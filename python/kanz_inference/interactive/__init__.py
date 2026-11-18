"""Interactive (sync) inference path — PRED-08.

The gRPC server side of PRED-06's ``inference.v1.InferenceService``.
Companion to ``kanz_inference.streaming`` (async path); both invoke
the same ``Model`` protocol but with different concurrency models:

- Streaming runs one asyncio task that fetches and handles one feature at
  a time. Pending features accumulate at the broker.
- Interactive RPCs share an in-flight counter. The servicer returns a
  DEGRADED response with ``pool_saturated`` when an arriving handler observes
  that limit. Work that occupies the event loop can also delay admission.

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
