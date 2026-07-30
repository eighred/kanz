# syntax=docker/dockerfile:1
#
# oms container (CICD-01c). Distroless + static binary.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/oms/Dockerfile .
#
# # There is exactly one OMS image
#
# There used to be two, and the difference between them was invisible at runtime:
# an untagged build compiled in NO exchange connector and silently routed every
# order to a simulator, while looking perfectly healthy. That trap is gone.
#
# After INFRA-M7a there are no venue build tags at all. Every exchange is an
# out-of-process adapter (services/venue-binance, services/venue-okx) selected by
# OMS_VENUE_ENDPOINTS at runtime, so this image links no vendor SDK, no request
# signing, and no exchange websocket — and it is the SAME image whether it trades
# Binance, OKX, both, or nothing. What it trades is a deployment decision, not a
# compile-time one, and a MIC with no adapter is a hard error at the router rather
# than a quiet simulation.
FROM ghcr.io/eighred/base/golang:1.26.5 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

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
FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/oms /oms
USER nonroot:nonroot
EXPOSE 8090
ENTRYPOINT ["/oms"]
