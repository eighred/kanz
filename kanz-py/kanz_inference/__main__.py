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
from kanz_bus.nats import NATSClient
from kanz_bus.producer import Producer, ProducerConfig

from kanz_inference.interactive.servicer import serve
from kanz_inference.publish import Publisher
from kanz_inference.service import NoPrimaryModelError, RegistryRouter, build_registry, load_specs, require_primary
from kanz_inference.streaming.worker import StreamingWorker

logger = logging.getLogger("kanz_inference")

DEFAULT_BIND = "[::]:50051"
DEFAULT_GROUP = "inference"


def _env(key: str, default: str = "") -> str:
    return os.environ.get(key, default).strip()


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

    # ONE router, BOTH paths. The interactive answer and the streaming answer are
    # produced by the same primary, so they cannot drift apart.
    router = RegistryRouter(registry, explain=_env("KANZ_INFERENCE_EXPLAIN", "true") != "false")

    bind = _env("KANZ_INFERENCE_BIND", DEFAULT_BIND)
    max_in_flight = int(_env("KANZ_INFERENCE_MAX_IN_FLIGHT", "64"))
    server = await serve(router, bind, max_in_flight=max_in_flight)

    stopping = asyncio.Event()
    tasks: list[asyncio.Task] = []

    nats_url = _env("KANZ_INFERENCE_NATS_URL")
    client: NATSClient | None = None
    if nats_url:
        client = NATSClient(url=nats_url, name=_env("KANZ_INFERENCE_SOURCE", "inference"))
        await client.connect()
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

    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, stopping.set)
        except NotImplementedError:  # Windows
            pass

    await stopping.wait()
    logger.info("shutting down")
    for t in tasks:
        t.cancel()
    await server.stop(grace=10)
    if client is not None:
        await client.close()
    return 0


def main() -> int:
    try:
        return asyncio.run(_run())
    except KeyboardInterrupt:
        return 0


if __name__ == "__main__":
    sys.exit(main())
