"""The inference service entrypoint (AI-M1).

There wasn't one. ``kanz_inference`` shipped a gRPC servicer, a streaming worker,
a feature store, an explainer, a model registry with a promotion gate, shadow
execution, validation and governance — and no way to start any of it. No
``__main__``, no Dockerfile, no manifest. Nothing on the platform served
``inference.v1.InferenceService``, and on the Go side ``internal/prediction`` had
zero importers outside its own tests.

This file starts BOTH paths over ONE registry, so the interactive answer and the
streaming answer can never disagree about which model is primary:

  * INTERACTIVE — the gRPC servicer (``interactive/servicer.py``), with the
    admission-control bound it already implements. This is what the Go
    ``prediction.SyncClient`` calls.
  * STREAMING — the bus worker (``streaming/worker.py``), consuming the
    ``inference.feature.computed`` FeatureVectors the Go engine publishes and
    emitting ``inference.prediction.scored`` FACTs.

It REFUSES TO START without a validated primary model. That is the same posture
the rest of this platform takes at every gate that matters — the gateway will not
start unauthenticated (SEC-M1), webhook-ingest will not start without a replay
defence (EXEC-M17/M22), tv-sync will not start without a fact log (EXEC-M21). A
prediction service with no model is not degraded; it is a healthy-looking process
that answers anyway.
"""

from __future__ import annotations

import asyncio
import logging
import os
import signal
import sys

from kanz_bus.consumer import Consumer
from kanz_bus import mtls
from kanz_bus.nats import NATSClient
from kanz_bus.producer import Producer, ProducerConfig

from kanz_inference.interactive.servicer import serve
from kanz_inference.observability import DEFAULT_METRICS_BIND, ObservabilityServer, metrics
from kanz_inference.publish import Publisher
from kanz_inference.service import NoPrimaryModelError, RegistryRouter, build_registry, load_specs, require_primary
from kanz_inference.streaming.worker import StreamingWorker

logger = logging.getLogger("kanz_inference")

DEFAULT_BIND = "[::]:50051"
DEFAULT_GROUP = "inference"


def _env(key: str, default: str = "") -> str:
    return os.environ.get(key, default).strip()


# How often to check whether spiffe-helper has rewritten the SVID. Minutes, not
# seconds: rotation happens on the order of an SVID lifetime, and the cost of
# being a few minutes late is nil — the loaded certificate is still valid.
SVID_POLL_SECONDS = 300.0


async def _watch_svid(ctx, cert_dir: str, stopping: asyncio.Event) -> None:
    """Keep the TLS context's SVID current while the process runs.

    Without this the context holds the certificate loaded at startup. The
    connection survives rotation (no re-handshake), so nothing fails — until the
    pod reconnects after that certificate expired, presents it, and is refused.
    nats-py then retries forever with the same dead cert and the streaming path
    never returns. See kanz_bus.mtls.reload_if_rotated.
    """
    fingerprint = mtls.reload_if_rotated(ctx, cert_dir, None)
    while not stopping.is_set():
        try:
            await asyncio.wait_for(stopping.wait(), timeout=SVID_POLL_SECONDS)
            return
        except asyncio.TimeoutError:
            pass
        try:
            updated = mtls.reload_if_rotated(ctx, cert_dir, fingerprint)
        except Exception:
            # A rotation we could not apply is worth a stack trace: the pod keeps
            # serving on the old SVID and will fail at the next reconnect after
            # expiry, which is far from here in time and hard to attribute.
            logger.exception("SVID reload failed — still serving on the previously loaded SVID")
            continue
        if updated != fingerprint:
            logger.info("SVID rotated; the next bus handshake uses the new certificate")
            fingerprint = updated


async def _run() -> int:
    logging.basicConfig(
        level=_env("KANZ_INFERENCE_LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )

    models_path = _env("KANZ_INFERENCE_MODELS")
    if not models_path:
        logger.error(
            "KANZ_INFERENCE_MODELS is required (path to the model-card JSON). "
            "Refusing to start: a prediction service with no model reports healthy and answers anyway."
        )
        return 2

    try:
        registry = build_registry(load_specs(models_path))
        require_primary(registry)
    except NoPrimaryModelError as e:
        logger.error("%s", e)
        return 2
    except Exception as e:  # a failed/expired validation lands here — the promotion gate
        logger.error(
            "no model could be promoted to PRIMARY (%s). A model serves production only with "
            "recorded, current validation (MLOPS-01a / SR 11-7).",
            e,
        )
        return 2

    # WHICH WEIGHTS IS THIS REPLICA SERVING. The promotion gate above already
    # refused anything without current validation, so this is not a health check
    # — it is the answer a risk desk needs when two replicas disagree, and it is
    # unavailable from outside the pod without a series.
    for meta in registry.list_models():
        metrics.PRIMARY_MODEL.labels(model_id=meta.model_id).set(1)

    # ONE router, BOTH paths. The interactive answer and the streaming answer are
    # produced by the same primary, so they cannot drift apart.
    router = RegistryRouter(registry, explain=_env("KANZ_INFERENCE_EXPLAIN", "true") != "false")

    bind = _env("KANZ_INFERENCE_BIND", DEFAULT_BIND)
    max_in_flight = int(_env("KANZ_INFERENCE_MAX_IN_FLIGHT", "64"))
    server = await serve(router, bind, max_in_flight=max_in_flight)

    # THE HTTP SURFACE COMES UP AFTER THE gRPC PORT IS BOUND, and that ordering is
    # the whole readiness contract. Binding 50051 already means "a PRIMARY model
    # is loaded" (the process exits 2 otherwise), so starting here means /livez
    # can never answer for a process that has no model to serve — the property
    # the old tcpSocket probe had, kept.
    #
    # It RAISES if the port cannot be bound, and that is not swallowed: a pod
    # carrying prometheus.io/port for a port nothing listens on is an
    # unreachable target, and an absent target appears on no dashboard.
    observability = ObservabilityServer(_env("KANZ_INFERENCE_METRICS_BIND", DEFAULT_METRICS_BIND))
    observability.start()

    stopping = asyncio.Event()
    tasks: list[asyncio.Task] = []

    nats_url = _env("KANZ_INFERENCE_NATS_URL")
    client: NATSClient | None = None
    if nats_url:
        # SEC-M3 (#241): present this pod's SVID to the broker. The production
        # broker sets `verify: true` + `verify_and_map`, so WITHOUT this the
        # streaming path cannot connect at all — and it is the wired half:
        # risk-engine really does publish inference.feature.computed.
        #
        # A missing cert dir is FATAL, not a downgrade to plaintext. A client that
        # silently falls back reports itself healthy and then either fails every
        # handshake, or — against a broker still accepting plaintext — connects
        # under NO identity, which is the state this change exists to remove.
        tls_ctx = None
        cert_dir = _env("KANZ_INFERENCE_NATS_CERT_DIR")
        if cert_dir:
            tls_ctx = mtls.client_context(cert_dir)
            logger.info("bus transport: mTLS, SVID from %s", cert_dir)
        else:
            logger.warning(
                "KANZ_INFERENCE_NATS_CERT_DIR is unset - connecting to the bus in PLAINTEXT, "
                "with no SPIFFE identity. A production broker (verify: true) refuses this "
                "connection; a broker that accepts it maps this client to no tenancy account."
            )
        client = NATSClient(
            url=nats_url,
            name=_env("KANZ_INFERENCE_SOURCE", "inference"),
            tls=tls_ctx,
        )
        await client.connect()
        if tls_ctx is not None:
            tasks.append(
                asyncio.create_task(
                    _watch_svid(tls_ctx, cert_dir, stopping), name="svid-rotation"
                )
            )
        producer = Producer(
            client,
            ProducerConfig(
                source=_env("KANZ_INFERENCE_SOURCE", "inference"),
                producer_version=_env("KANZ_INFERENCE_VERSION", "dev"),
            ),
        )
        worker = StreamingWorker(Consumer(client), router, Publisher(producer))
        group = _env("KANZ_INFERENCE_CONSUMER_GROUP", DEFAULT_GROUP)
        tasks.append(asyncio.create_task(worker.run(group), name="streaming-worker"))
        logger.info("streaming worker consuming inference.feature.computed (group=%s)", group)
    else:
        # An explicit, loud gap — not a silent one. The gRPC path still serves, but
        # nothing will consume the FeatureVectors the Go engine publishes.
        logger.warning(
            "KANZ_INFERENCE_NATS_URL is unset — the STREAMING path is off. "
            "FeatureVector FACTs on the bus will not be scored."
        )

    # BOTH paths are wired; only now does this pod claim to be ready.
    observability.mark_ready()

    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, stopping.set)
        except NotImplementedError:  # Windows
            pass

    await stopping.wait()
    logger.info("shutting down")
    # 503 ON /readyz FIRST, AND THIS IS WHY THE PROBES ARE NO LONGER tcpSocket.
    # The listening socket stays open until the process exits, so a TCP readiness
    # check passes for the whole termination grace period and the endpoints
    # controller keeps sending this pod work it is about to drop. Failing
    # readiness before anything is torn down is what removes it from the Service
    # while it can still finish what it has.
    observability.mark_draining()
    for t in tasks:
        t.cancel()
    await server.stop(grace=10)
    if client is not None:
        await client.close()
    observability.stop()
    return 0


def main() -> int:
    try:
        return asyncio.run(_run())
    except KeyboardInterrupt:
        return 0


if __name__ == "__main__":
    sys.exit(main())
