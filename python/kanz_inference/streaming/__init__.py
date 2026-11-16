"""Streaming inference worker — PRED-04.

The async bus-consumer side of the prediction layer. Receives
``inference.feature.computed`` events, decodes the FeatureVector
payload, calls the configured ``Model.predict`` (with degraded
fallback per PRED-02), and hands the resulting PredictionEnvelope
to a ``PredictionPublisher`` (PRED-05 will satisfy this protocol).
"""

from kanz_inference.streaming.worker import (
    LastKnownCache,
    Model,
    PredictionPublisher,
    StreamingWorker,
)

__all__ = [
    "LastKnownCache",
    "Model",
    "PredictionPublisher",
    "StreamingWorker",
]
