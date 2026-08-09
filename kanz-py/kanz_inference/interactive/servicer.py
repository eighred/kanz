"""Interactive inference servicer + admission control (PRED-08).

Implements ``inference.v1.InferenceService.Predict`` (PRED-06) as a
grpc-aio servicer with a bounded in-flight counter. Over-limit
requests are admit-rejected with a DEGRADED PredictionEnvelope
(``degraded_reason = "pool_saturated"``) per PRED-02 §1's
never-black-hole rule — same shape PRED-04 / PRED-07 return for
their fallback paths.

# Why a counter, not asyncio.Semaphore

Semaphore acquire() blocks until a slot is free; that defeats
admission control (the whole point is to fail fast, not queue
indefinitely). asyncio doesn't expose a non-blocking try_acquire
on stdlib Semaphore. A plain counter under asyncio is race-free
(single event loop, no `await` between read and increment) and
gives the exact "in-flight ≥ max ⇒ reject immediately" semantic
admission control needs.

# Server-side timeout / cancellation

Server-side timeouts are NOT enforced here. PRED-07's client
already enforces a per-call deadline; when it expires the gRPC
framework cancels the server-side coroutine and we propagate the
``asyncio.CancelledError``. Adding a server-side timeout on top
would race with the client deadline; cleaner to defer to the
client. If the client doesn't set a deadline (a bug per PRED-06
contract), the server-side call runs to completion — surface this
in observability rather than silently capping it.
"""

from __future__ import annotations

import asyncio
import logging
import time
from typing import Optional

import grpc

from kanz_bus import mtls

from envelope.v1.envelope_pb2 import QualityFlag  # noqa: F401 (reserved for future flag-on-envelope work)
from inference.v1.feature_vector_pb2 import FeatureVector
from inference.v1.inference_service_pb2_grpc import (
    InferenceServiceServicer,
    add_InferenceServiceServicer_to_server,
)
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.observability import metrics
from kanz_inference.streaming.worker import Model

logger = logging.getLogger(__name__)

# Default admission limit. 32 = conservative default for a single
# Python worker on a typical container size; tune per deployment
# based on observed p99 latency × target QPS.
DEFAULT_MAX_IN_FLIGHT = 32

# Degraded-reason identifiers from PRED-02 §2.1, mirrored from
# PRED-04's streaming worker so server-side and worker-side
# observability agree on the strings.
REASON_INFERENCE_UNAVAILABLE = "inference_unavailable"
REASON_POOL_SATURATED = "pool_saturated"


class InferenceServicer(InferenceServiceServicer):
    """gRPC servicer implementing inference.v1.InferenceService.Predict.

    Construct with a Model + admission limit; register with a
    grpc.aio.Server via ``add_InferenceServiceServicer_to_server``
    (the ``serve`` helper at module level does this for the
    common case).

    Thread-safety: single asyncio event loop only. Running this
    behind a thread-pool gRPC executor would race the in-flight
    counter — stay on grpc.aio.
    """

    def __init__(self, model: Model, max_in_flight: int = DEFAULT_MAX_IN_FLIGHT):
        if model is None:
            raise ValueError("model required")
        if max_in_flight <= 0:
            raise ValueError("max_in_flight must be > 0")
        self._model = model
        self._max_in_flight = max_in_flight
        self._in_flight = 0
        # The bound is what kanz_inference_predict_in_flight has to be read
        # against — a gauge of 30 is healthy at 64 and refusing traffic at 32,
        # and an alert cannot tell those apart without the denominator.
        metrics.ADMISSION_LIMIT.set(max_in_flight)

    @property
    def in_flight(self) -> int:
        """Current number of pending Predict calls. Exposed for
        observability + tests."""
        return self._in_flight

    async def Predict(
        self, request: FeatureVector, context: Optional[grpc.aio.ServicerContext]
    ) -> PredictionEnvelope:
        if self._in_flight >= self._max_in_flight:
            logger.warning(
                "admission rejected for %s: pool saturated (%d/%d)",
                request.subject_id,
                self._in_flight,
                self._max_in_flight,
            )
            # THE ADMISSION-REJECT SERIES. It is counted here rather than by a
            # dedicated counter because a reject IS a degraded response — see the
            # note on metrics.PREDICT_IN_FLIGHT. NOTE the early return: this path
            # never enters the timing block below, so a reject cannot flatter the
            # latency histogram at the exact moment the pod is saturated.
            return metrics.observe_prediction(
                metrics.PATH_INTERACTIVE, self._degraded(request, REASON_POOL_SATURATED)
            )

        self._in_flight += 1
        metrics.PREDICT_IN_FLIGHT.set(self._in_flight)
        started = time.perf_counter()
        try:
            return metrics.observe_prediction(
                metrics.PATH_INTERACTIVE, await self._model.predict(request)
            )
        except asyncio.CancelledError:
            # Client deadline expired or call cancelled. Don't
            # synthesise a response — the client isn't listening.
            # PRED-07 translates DEADLINE_EXCEEDED to a DEGRADED
            # envelope on the client side per PRED-06 contract.
            #
            # Counted as its own mode, NOT as degraded: no envelope was produced
            # and the fault is a client deadline, not the model. Folding it into
            # the degraded rate would page the model owner for a caller's timeout.
            metrics.PREDICTIONS.labels(
                path=metrics.PATH_INTERACTIVE, mode=metrics.MODE_CANCELLED
            ).inc()
            raise
        except Exception as e:  # noqa: BLE001 — degraded-fallback path
            logger.warning(
                "model.predict raised for %s: %s — degraded response",
                request.subject_id,
                e,
            )
            return metrics.observe_prediction(
                metrics.PATH_INTERACTIVE,
                self._degraded(request, REASON_INFERENCE_UNAVAILABLE),
            )
        finally:
            self._in_flight -= 1
            metrics.PREDICT_IN_FLIGHT.set(self._in_flight)
            # Observed for every ADMITTED call, including the failed and cancelled
            # ones: a model that fails slowly is the case where the p99 matters
            # most, and dropping those samples hides it.
            metrics.PREDICT_SECONDS.labels(path=metrics.PATH_INTERACTIVE).observe(
                time.perf_counter() - started
            )

    def _degraded(
        self, fv: FeatureVector, reason: str
    ) -> PredictionEnvelope:
        """Build a zero-value DEGRADED response per PRED-02 §2.1.
        Carries the would-have-been model_id on the envelope for
        traceability (which model was supposed to serve this).
        """
        return PredictionEnvelope(
            subject_id=fv.subject_id,
            model=self._model.model_id,
            value=0.0,
            confidence=0.0,
            mode=PredictionMode.PREDICTION_MODE_DEGRADED,
            degraded_reason=reason,
            as_of=fv.as_of,
        )


class InsecureServicerRefused(RuntimeError):
    """Raised when the servicer is asked to serve plaintext without saying so.

    Refusing is the point. Every Go gRPC server in this estate that lacks mTLS
    material logs a warning and serves plaintext anyway, and the result is a
    surface nobody notices is unprotected. This one will not start.
    """


def _server_credentials(cert_dir: str) -> grpc.ServerCredentials:
    """mTLS credentials from the SVID spiffe-helper materialised (#241).

    ``require_client_auth=True`` is the half that matters: without it the server
    presents a certificate and accepts ANY caller, which reads as "TLS is on" in
    every dashboard and log while authenticating nobody.

    The PEM paths and their fail-loud check are reused from :mod:`kanz_bus.mtls`
    so the servicer and the bus client cannot disagree about where the SVID lives
    — one definition, one set of file names.
    """
    paths = mtls.SVIDPaths.in_dir(cert_dir)
    if missing := paths.missing():
        raise mtls.MissingSVIDError(
            "grpc: SVID material missing: "
            + ", ".join(missing)
            + " — the spiffe-helper sidecar has not written the SVID, so this surface has no "
            "identity to present."
        )
    with open(paths.key, "rb") as f:
        key = f.read()
    with open(paths.cert, "rb") as f:
        chain = f.read()
    with open(paths.bundle, "rb") as f:
        bundle = f.read()
    return grpc.ssl_server_credentials(
        [(key, chain)],
        root_certificates=bundle,
        require_client_auth=True,
    )


async def serve(
    model: Model,
    bind_address: str,
    max_in_flight: int = DEFAULT_MAX_IN_FLIGHT,
    cert_dir: str = "",
    allow_insecure: bool = False,
) -> grpc.aio.Server:
    """Start a grpc.aio server bound to ``bind_address`` (e.g.
    ``"[::]:50051"``) with ``InferenceServicer(model, max_in_flight)``
    registered. Returns the server so the caller can ``await
    server.wait_for_termination()`` (typical) or shut it down
    programmatically.

    With ``cert_dir`` the listener is mutual TLS. WITHOUT it, and without an
    explicit ``allow_insecure``, this REFUSES to start rather than serving
    plaintext — see :class:`InsecureServicerRefused`.
    """
    server = grpc.aio.server()
    servicer = InferenceServicer(model, max_in_flight=max_in_flight)
    add_InferenceServiceServicer_to_server(servicer, server)

    if cert_dir:
        server.add_secure_port(bind_address, _server_credentials(cert_dir))
        transport = "mTLS"
    elif allow_insecure:
        # The dev-only escape hatch, in the shape COPILOT_ALLOW_STUB already uses
        # in this estate: possible, explicit, and never the fix for a failing
        # deploy. It is deliberately absent from inference-deploy.yaml.
        server.add_insecure_port(bind_address)
        transport = "PLAINTEXT (KANZ_INFERENCE_ALLOW_INSECURE_GRPC=true)"
        logger.warning(
            "the prediction surface is serving PLAINTEXT on %s with no client authentication — "
            "any workload that can reach the port can ask this model for predictions. This is a "
            "local-development setting and must never be set in a deployment.",
            bind_address,
        )
    else:
        raise InsecureServicerRefused(
            "grpc: no KANZ_INFERENCE_GRPC_CERT_DIR and KANZ_INFERENCE_ALLOW_INSECURE_GRPC is not "
            "set — refusing to serve the prediction surface in plaintext. Set the cert dir (the "
            "pod mounts an SVID at /etc/inference-certs), or opt into plaintext explicitly for "
            "local development."
        )

    await server.start()
    logger.info(
        "inference servicer listening on %s (transport=%s, max_in_flight=%d, model=%s)",
        bind_address,
        transport,
        max_in_flight,
        model.model_id,
    )
    return server
