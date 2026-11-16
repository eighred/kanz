"""Python prediction layer — streaming worker (PRED-04), publish
(PRED-05), interactive worker pool (PRED-08), model registry
(PRED-09), shadow / canary (PRED-10), feature-store boundary
(PRED-11).

Symmetric counterpart to ``kanz/internal/prediction`` on the Go side:
features arrive from Go via the bus, models score them in Python,
predictions return to Go via the bus (streaming) or gRPC (sync).

Importable submodules:

- ``kanz_inference.streaming`` — streaming bus-consumer worker
  (PRED-04). Drives ``Model.predict`` on every inbound
  ``inference.feature.computed`` event and hands the result to a
  ``PredictionPublisher`` (PRED-05).
"""
