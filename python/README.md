# Python bus and inference components

This directory contains `kanz_bus`, the asynchronous NATS JetStream/Kafka client,
and `kanz_inference`, the inference service. They currently share the `kanz-bus`
Python distribution. Renaming the source directory does not rename that package.
The Go bus and canonical Protobuf contracts define wire compatibility.

Inference serves the existing typed gRPC API and can consume feature FACTs on
NATS. Both paths share a model registry. Startup refuses a missing or unvalidated
primary model; request-time unsupported features return explicit degraded state.
Models and AI output never authorize orders or replace risk/compliance/mandate
checks. The gateway/domain services remain authorization owners.

## Development setup

Use Python 3.12 for the shipped runtime. The package metadata allows 3.10+;
that range is not evidence that every native wheel works on every interpreter.
Start in the repository root with a virtual environment. Generate SDKs using
[the schema guide](../schemas/README.md); generated Python uses namespace
packages and is not an editable install until release packaging is applied.

```sh
python -m venv .venv
# Activate the virtual environment for your shell.
python -m pip install --require-hashes --only-binary=:all: -r python/requirements.txt
python -m pip install pytest pytest-asyncio cryptography
export PYTHONPATH="$PWD/schemas/gen/python:$PWD/python"
python -m pytest python/tests -q
```

In PowerShell, set `$env:PYTHONPATH` to the absolute SDK and Python roots joined
by `;`. In POSIX shells use `:` as above. The hash-pinned runtime and codegen
locks are separate; `requirements-codegen.txt` builds the image SDK in its own
Python environment. CI also checks that generated stubs import under the pinned
runtime. The `kanz-schemas` distribution name remains unchanged.

For cross-language serialization parity, generate the synthetic Go corpus from
`kanz/` before pytest: `go run ./test/contract/serialization/cmd/genfixtures
-out ./test/contract/serialization/fixtures` (one command). See
[the fixture guide](../kanz/test/contract/serialization/fixtures/README.md).

Unit tests use local fixtures. `TEST_NATS_URL` and `TEST_KAFKA_BROKERS` enable
real broker suites; unset values skip those integrations. Use disposable brokers
and report skips. Python dependency and image tests are in
[kanz-ci.yml](../.github/workflows/kanz-ci.yml).

### Measure inference interference

With the generated SDK and dependencies above, set `KANZ_TEST_NATS_SERVER` to a
local NATS server executable and run `python -B python/tests/test_inference_interference.py -v`
from the repository root. The fixture starts its own loopback JetStream and inference
process in disposable storage. It scores a synthetic two-feature linear model with
explanations and an interactive admission limit of one. It compares 128 RPCs and
128 feature FACTs separately and together at up to 16 concurrent RPCs, then checks
that a smaller batch succeeds after the durable consumer drains.

The JSON report includes client-observed RPC p95/p99, time from feature publication
start to scored FACT receipt, streaming throughput, overlapping completions,
admission refusals and final pending acknowledgements. The fixture checks model
identity, numerical outputs and committed message counts. Latency and throughput
are observations for the named workload. A production capacity contract and pool
placement remain decisions under #1348 and #1545.

## Run inference

Run `python -m kanz_inference` with the same `PYTHONPATH`. Configuration is read
in `kanz_inference/__main__.py`:

| Setting | Responsibility |
|---|---|
| `KANZ_INFERENCE_MODELS` | Required path to reviewed model-card JSON; startup requires a validated primary. |
| `KANZ_INFERENCE_BIND` | gRPC listener, default `[::]:50051`. |
| `KANZ_INFERENCE_GRPC_CERT_DIR` | Workload certificate directory for gRPC mTLS. |
| `KANZ_INFERENCE_METRICS_BIND` | HTTP metrics/liveness/readiness surface, default `[::]:8093`. |
| `KANZ_INFERENCE_NATS_URL`, `KANZ_INFERENCE_NATS_CERT_DIR` | Optional streaming connection and workload mTLS material. |
| `KANZ_INFERENCE_MAX_IN_FLIGHT` | Bounded interactive admission, default 64. |

The insecure gRPC option is for explicitly isolated fixtures. Missing NATS
configuration disables streaming; it does not establish broker integration.
Keep model approval and credentials out of source and agent context. The Docker
build uses the repository root: `docker build -f python/Dockerfile .`.
It generates its own SDK and installs the hash-locked runtime.

## Bus API

`kanz_bus` exposes `Message`, `NATSClient` and `KafkaClient`; publication and
consumption also use the envelope-aware `Producer` and `Consumer`. See the
implementation and tests for required schema references, producer version,
lineage and tenant routing. Arbitrary bytes are not a valid canonical FACT.
The Kafka-compatible path is derived archive/CDC; it is not a second live writer.
