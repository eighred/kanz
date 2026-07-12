# syntax=docker/dockerfile:1
#
# oms container (CICD-01c). Distroless + static binary.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/kanz-eng/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/oms/Dockerfile .
#
# # GOTAGS decides whether this image can trade
#
# The venue adapters are build-tag split (services/oms/cmd/oms/venues_*.go). The
# DEFAULT build is vendor-free: `//go:build !binance` and `!okx` compile in no
# exchange connector at all, leaving only the SimVenue. That default is
# deliberate — it keeps the vendor SDKs and the IP-hardened connector code out of
# an artifact that does not need them.
#
# It also means an untagged image is a SIMULATOR. It starts, passes its probes,
# accepts orders, and routes every one of them to a fake venue — successfully,
# with no error, looking healthy on every dashboard. Deploying this image and
# believing it is trading is a silent, total failure.
#
# So a real-venue image is an EXPLICIT act:
#
#   docker build --build-arg GOTAGS=binance,okx -f kanz/services/oms/Dockerfile .
#
# and the resulting image must be tagged so that nobody can mistake one for the
# other. If oms-deploy.yaml points at an untagged build, the OMS is not trading.
FROM golang:1.26.1 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

# Empty = the vendor-free, simulation-only default. See above.
ARG GOTAGS=""

# Dependency layer: go.mod/go.sum + the generated SDK the replace points at,
# so `go mod download` is cached across source-only changes.
COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
RUN go mod download

COPY kanz/ ./
# CGO off ⇒ a fully static binary (every dep — pgx, nats, kafka-go, go-spiffe —
# is pure Go), which is what distroless/static requires. -trimpath strips local
# paths; -s -w drop the symbol/debug tables.
RUN CGO_ENABLED=0 go build -tags "${GOTAGS}" -trimpath -ldflags="-s -w" \
      -o /out/oms ./services/oms/cmd/oms

# Distroless static: no shell, no libc, no package manager — minimal attack
# surface. :nonroot runs as uid 65532, matching the pod's runAsUser.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/oms /oms
USER nonroot:nonroot
EXPOSE 8090
ENTRYPOINT ["/oms"]
