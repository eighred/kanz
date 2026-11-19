"""Bounded #1348 comparison on a private loopback broker and the real inference entrypoint."""
import asyncio
import hashlib
import http.client
import importlib.metadata
import json
import math
import os
import platform
import socket
import subprocess
import sys
import tempfile
import time
import unittest
from contextlib import contextmanager
from datetime import datetime, timedelta, timezone
from pathlib import Path

import grpc
import nats
from nats.js.api import StorageType, StreamConfig
from nats.js.errors import NotFoundError

from envelope.v1.event_class_pb2 import EventClass
from inference.v1.feature_vector_pb2 import FeatureVector
from inference.v1.inference_service_pb2_grpc import InferenceServiceStub
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode
from kanz_bus import Event, NATSClient, Producer, ProducerConfig, unframe, validate
from kanz_inference.interactive.servicer import REASON_POOL_SATURATED
from kanz_inference.publish import SUBJECT_PREDICTION_SCORED
from kanz_inference.streaming.worker import SUBJECT_FEATURE_COMPUTED


MODEL = "fixture-interference@1"
SET = "portfolio-risk:1"
GROUP = "interference-proof"
COUNT = 128
RPC_CONCURRENCY = 16


@contextmanager
def process(args, **kwargs):
    child = subprocess.Popen(args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, **kwargs)
    try:
        yield child
    finally:
        child.terminate()
        try:
            child.wait(timeout=5)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait(timeout=5)


def local_ports():
    # Hold all sockets together so the three chosen loopback ports are distinct.
    socks = [socket.socket() for _ in range(3)]
    try:
        for sock in socks:
            sock.bind(("127.0.0.1", 0))
        return [sock.getsockname()[1] for sock in socks]
    finally:
        for sock in socks:
            sock.close()


def vector(subject):
    fv = FeatureVector(subject_id=subject, feature_set_ref=SET)
    fv.as_of.FromDatetime(datetime.now(timezone.utc))
    fv.values["GrossExposure"].scalar = 5.0
    fv.values["VaR99"].scalar = 1.0
    return fv


def check_prediction(prediction, fv):
    if (prediction.subject_id != fv.subject_id or prediction.as_of != fv.as_of
            or prediction.model != MODEL or prediction.mode != PredictionMode.PREDICTION_MODE_NORMAL
            or not math.isclose(prediction.value, 1.2, abs_tol=1e-12)
            or not math.isclose(prediction.explanation["GrossExposure"], 2.0, abs_tol=1e-12)
            or not math.isclose(prediction.explanation["VaR99"], -0.9, abs_tol=1e-12)):
        raise AssertionError("prediction identity, independent linear score or attribution mismatch")


def summary(samples):
    if not samples:
        return {}
    ordered = sorted(samples)
    return {f"p{p}_ms": ordered[math.ceil(p * len(ordered) / 100) - 1] * 1000 for p in (95, 99)}


def ready_status(port):
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=1)
    try:
        connection.request("GET", "/readyz")
        return connection.getresponse().status
    except OSError:
        return None
    finally:
        connection.close()


async def phase(name, rpc_n, stream_n, stub, producer, scored, js):
    initial_messages = (await js.stream_info("INFERENCE")).state.messages
    rpc_times, stream_times, stream_start = [], [], {}
    refusals = 0
    active_rpc = peak_rpc = rpc_during_stream = stream_during_rpc = 0
    seen = set()
    launch, finish = {}, {}
    pool = asyncio.Semaphore(RPC_CONCURRENCY)

    async def call(index):
        nonlocal refusals, active_rpc, peak_rpc, rpc_during_stream
        async with pool:
            fv = vector(f"{name}-r-{index}")
            started = time.perf_counter()
            active_rpc += 1
            peak_rpc = max(peak_rpc, active_rpc)
            try:
                response = await stub.Predict(fv, timeout=10)
                rpc_during_stream += int(len(stream_start) > len(seen))
            finally:
                active_rpc -= 1
            rpc_times.append(time.perf_counter() - started)
            if (response.mode == PredictionMode.PREDICTION_MODE_DEGRADED
                    and response.degraded_reason == REASON_POOL_SATURATED):
                if response.subject_id != fv.subject_id or response.as_of != fv.as_of:
                    raise AssertionError("unattributed admission refusal")
                refusals += 1
            else:
                check_prediction(response, fv)

    async def interactive():
        if rpc_n:
            launch["rpc"] = time.perf_counter()
            await asyncio.gather(*(call(i) for i in range(rpc_n)))
            finish["rpc"] = time.perf_counter()

    async def publish():
        for i in range(stream_n):
            fv = vector(f"{name}-s-{i}")
            start = time.perf_counter()
            stream_start[fv.subject_id] = (start, fv)
            await producer.publish(Event(
                subject=SUBJECT_FEATURE_COMPUTED, event_type=SUBJECT_FEATURE_COMPUTED,
                event_class=EventClass.EVENT_CLASS_FACT, schema_version=1, domain="inference",
                event_time=fv.as_of.ToDatetime(timezone.utc), payload_schema_ref="inference.v1.FeatureVector:1",
                partition_key=fv.subject_id, payload=fv, correlation_id=f"fixture-{fv.subject_id}",
            ))

    async def receive():
        nonlocal stream_during_rpc
        for _ in range(stream_n):
            msg = await scored.next_msg(timeout=10)
            arrived = time.perf_counter()
            stream_during_rpc += int(active_rpc > 0)
            env, payload = unframe(msg.data)
            validate(env)
            result = PredictionEnvelope.FromString(payload)
            if result.subject_id in seen or result.subject_id not in stream_start:
                raise AssertionError("foreign or duplicate scored feature")
            seen.add(result.subject_id)
            started, fv = stream_start[result.subject_id]
            check_prediction(result, fv)
            if (env.tenant_id != "fixture-tenant" or not env.causation_id
                    or env.correlation_id != f"fixture-{result.subject_id}"):
                raise AssertionError("prediction lost feature lineage")
            stream_times.append(arrived - started)

    async def streaming():
        if stream_n:
            launch["stream"] = time.perf_counter()
            await asyncio.gather(publish(), receive())
            finish["stream"] = time.perf_counter()

    await asyncio.gather(interactive(), streaming())
    overlap = min(finish.values()) - max(launch.values()) if rpc_n and stream_n else 0.0
    if (rpc_n and stream_n and overlap <= 0) or len(seen) != stream_n:
        raise AssertionError("paths failed to overlap or complete")
    if name == "mixed" and not (rpc_during_stream and stream_during_rpc):
        raise AssertionError("mixed phase did not observe completions across both outstanding paths")
    # Confirm JetStream has drained all feature work before measuring recovery.
    while True:
        state = await js.consumer_info("INFERENCE", GROUP)
        if not state.num_pending and not state.num_ack_pending:
            break
        await asyncio.sleep(0.01)
    if rpc_n and refusals == rpc_n:
        raise AssertionError("no successful interactive prediction")
    committed = (await js.stream_info("INFERENCE")).state.messages - initial_messages
    if committed != 2 * stream_n:
        raise AssertionError("unexpected number of committed feature and prediction FACTs")
    return {"phase": name, "rpc_count": len(rpc_times), "rpc_refused": refusals,
            "peak_client_rpc": peak_rpc, "rpc_completed_with_stream_outstanding": rpc_during_stream,
            "stream_completed_with_rpc_outstanding": stream_during_rpc,
            "rpc": summary(rpc_times), "stream_count": len(seen), "stream_lag": summary(stream_times),
            "stream_per_second": stream_n / (finish["stream"] - launch["stream"]) if stream_n else None,
            "overlap_ms": overlap * 1000, "committed_facts": committed, "final_pending": state.num_pending,
            "final_ack_pending": state.num_ack_pending}


async def measure(root, binary):
    nats_port, grpc_port, metrics_port = local_ports()
    now = datetime.now(timezone.utc)
    model = dict(model_id=MODEL, feature_set_ref=SET, confidence_threshold=0.5,
                 weights={"GrossExposure": 0.4, "VaR99": -0.9}, bias=0.1,
                 validated_at=now.isoformat(), expires_at=(now + timedelta(hours=1)).isoformat(),
                 validation_passed=True, report_uri="fixture://interference-only")
    card = root / "model.json"
    card.write_text(json.dumps(model), encoding="utf-8")
    url = f"nats://127.0.0.1:{nats_port}"
    env = {k: os.environ[k] for k in ("PATH", "PYTHONPATH", "SYSTEMROOT", "WINDIR") if k in os.environ}
    env.update(KANZ_INFERENCE_MODELS=str(card), KANZ_INFERENCE_NATS_URL=url,
               KANZ_INFERENCE_BIND=f"127.0.0.1:{grpc_port}", KANZ_INFERENCE_ALLOW_INSECURE_GRPC="true",
               KANZ_INFERENCE_METRICS_BIND=f"127.0.0.1:{metrics_port}", KANZ_INFERENCE_CONSUMER_GROUP=GROUP,
               KANZ_INFERENCE_EXPLAIN="true", KANZ_INFERENCE_MAX_IN_FLIGHT="1", PYTHONDONTWRITEBYTECODE="1")
    with process([binary, "-js", "-a", "127.0.0.1", "-p", str(nats_port), "-sd", str(root / "broker")]) as broker:
        while True:
            if broker.poll() is not None:
                raise RuntimeError("private broker failed to start")
            try:
                _, writer = await asyncio.open_connection("127.0.0.1", nats_port)
                writer.close()
                await writer.wait_closed()
                break
            except OSError:
                await asyncio.sleep(0.05)
        nc = await nats.connect(url)
        try:
            js = nc.jetstream()
            # A working NATS connection alone cannot serve streaming traffic.
            # Start before provisioning, then recover using the same model and endpoints.
            with process([sys.executable, "-B", "-m", "kanz_inference"], env=env) as unbound:
                deadline = time.monotonic() + 10
                while unbound.poll() is None and time.monotonic() < deadline:
                    if await asyncio.to_thread(ready_status, metrics_port) == 200:
                        raise AssertionError("reported ready without a feature stream")
                    await asyncio.sleep(0.025)
                if unbound.returncode != 1:
                    raise AssertionError(f"expected a streaming failure, got {unbound.returncode}")
            await js.add_stream(StreamConfig(name="INFERENCE", subjects=["inference.>"], storage=StorageType.MEMORY))
            scored = await nc.subscribe(SUBJECT_PREDICTION_SCORED)
            await nc.flush()
            with process([sys.executable, "-B", "-m", "kanz_inference"], env=env) as service:
                while True:
                    if service.poll() is not None:
                        raise RuntimeError("inference process failed to start")
                    try:
                        await js.consumer_info("INFERENCE", GROUP)
                        if await asyncio.to_thread(ready_status, metrics_port) == 200:
                            break
                        await asyncio.sleep(0.05)
                    except NotFoundError:
                        await asyncio.sleep(0.05)
                async with (grpc.aio.insecure_channel(f"127.0.0.1:{grpc_port}") as channel,
                            NATSClient(url=url, name="interference-producer") as bus):
                    await channel.channel_ready()
                    stub = InferenceServiceStub(channel)
                    emitter = Producer(bus, ProducerConfig(source="interference-fixture", producer_version="1",
                                                           tenant="fixture-tenant"))
                    results = []
                    for name, rpc, stream in (("warmup", 1, 1), ("interactive_only", COUNT, 0),
                                              ("streaming_only", 0, COUNT), ("mixed", COUNT, COUNT),
                                              ("recovery", 8, 8)):
                        results.append(await phase(name, rpc, stream, stub, emitter, scored, js))
                await js.delete_consumer("INFERENCE", GROUP)
                if await asyncio.to_thread(service.wait, timeout=25) != 1:
                    raise AssertionError("lost durable subscription without a process failure")
            with process([sys.executable, "-B", "-m", "kanz_inference"],
                         env={**env, "KANZ_INFERENCE_NATS_URL": ""}) as interactive:
                while await asyncio.to_thread(ready_status, metrics_port) != 200:
                    if interactive.poll() is not None:
                        raise RuntimeError("interactive-only service failed to start")
                    await asyncio.sleep(0.05)
                async with grpc.aio.insecure_channel(f"127.0.0.1:{grpc_port}") as channel:
                    fv = vector("interactive-only")
                    check_prediction(await InferenceServiceStub(channel).Predict(fv, timeout=5), fv)
                if os.name == "posix":
                    interactive.terminate()
                    if await asyncio.to_thread(interactive.wait, timeout=10) != 0:
                        raise AssertionError("SIGTERM did not shut down the service cleanly")
        finally:
            await nc.close()
    return {"model": MODEL, "card_sha256": hashlib.sha256(card.read_bytes()).hexdigest(),
            "source": subprocess.check_output(["git", "rev-parse", "HEAD"], text=True,
                                             cwd=Path(__file__).parent).strip(),
            "python": platform.python_version(),
            "nats_server": subprocess.check_output([binary, "-v"], text=True).strip(),
            "packages": {p: importlib.metadata.version(p) for p in ("grpcio", "nats-py", "protobuf")},
            "max_in_flight": 1, "client_concurrency": RPC_CONCURRENCY, "phases": results}


class InferenceInterference(unittest.IsolatedAsyncioTestCase):
    @unittest.skipUnless(os.environ.get("KANZ_TEST_NATS_SERVER"), "requires a private nats-server executable")
    async def test_shared_process(self):
        asyncio.get_running_loop().set_debug(False)
        with tempfile.TemporaryDirectory() as scratch:
            result = await asyncio.wait_for(measure(Path(scratch), os.environ["KANZ_TEST_NATS_SERVER"]), timeout=60)
        self.assertEqual(result["phases"][-1]["rpc_refused"], 0)
        print(json.dumps(result))


if __name__ == "__main__":
    unittest.main()
