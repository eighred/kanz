"""Shadow / canary inference (PRED-10).

ShadowExecutor wraps the primary + shadow Models registered in
PRED-09's Registry: predict returns the primary's response (the
served result), shadow models are called in parallel for
evaluation, results compared via a ShadowObserver. Shadow failures
NEVER affect the primary response — the canary's whole job is
risk-free evaluation.
"""

from kanz_inference.shadow.executor import (
    LoggingShadowObserver,
    ShadowExecutor,
    ShadowObserver,
)

__all__ = ["LoggingShadowObserver", "ShadowExecutor", "ShadowObserver"]
