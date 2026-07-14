# syntax=docker/dockerfile:1
#
# The inference service (AI-M1) — the first image this platform has ever had for its AI layer.
#
# kanz_inference shipped a gRPC servicer, a streaming worker, a point-in-time feature store, a
# permutation-Shapley explainer, a model registry with a promotion gate, shadow execution,
# validation and governance. It had NO ENTRYPOINT, NO DOCKERFILE, NO MANIFEST and NO CONCRETE
# MODEL, and on the Go side internal/prediction had zero importers outside its own tests. The
# platform's whole AI capability was an exceptionally well-specified contract that had never
# once run.
#
# Build context is the repo ROOT, not kanz-py/ — the Python schema SDK is GENERATED, NOT
# COMMITTED (EVT-15a), the same rule the Go images live under, so the build needs both trees:
#   docker build -f kanz-py/Dockerfile .
FROM python:3.12-slim AS build

# buf generates the Python SDK (protocolbuffers/python + pyi + grpc stubs). It is not in git,
# so it is generated here rather than copied — an image that COPYs a generated tree silently
# ships whatever was last on the builder's disk.
RUN pip install --no-cache-dir grpcio-tools==1.68.1
WORKDIR /src
COPY kanz-schemas/proto/ ./proto/
RUN mkdir -p /out/gen && \
    python -m grpc_tools.protoc \
      -I proto \
      --python_out=/out/gen \
      --pyi_out=/out/gen \
      --grpc_python_out=/out/gen \
      $(find proto -name '*.proto')

FROM python:3.12-slim
WORKDIR /app

# Runtime deps only. The ML stack is deliberately absent: the explainer is a hand-rolled
# permutation Shapley precisely so `shap` is not a dependency, and the first model is a linear
# scorer. A transformer or a GNN arriving later brings its own dependency and its own image
# layer — it does not get to bloat this one pre-emptively.
COPY kanz-py/pyproject.toml ./
RUN pip install --no-cache-dir \
      "nats-py>=2.7.0" "aiokafka>=0.10.0" "protobuf>=5.29" "grpcio>=1.62"

COPY --from=build /out/gen /app/gen
COPY kanz-py/kanz_bus/ ./kanz_bus/
COPY kanz-py/kanz_inference/ ./kanz_inference/

# The generated SDK is a sibling namespace root, exactly as it is for the tests.
ENV PYTHONPATH=/app/gen:/app
ENV PYTHONUNBUFFERED=1

# Non-root, matching every other pod on this platform (runAsUser 65532).
RUN useradd -u 65532 -m nonroot
USER 65532

EXPOSE 50051

# It REFUSES TO START without a validated PRIMARY model (KANZ_INFERENCE_MODELS). That is not a
# convenience check: a prediction service with no model does not fail — it reports healthy and
# it answers, which is the one outcome worse than being down.
ENTRYPOINT ["python", "-m", "kanz_inference"]
