# syntax=docker/dockerfile:1
#
# webhook-ingest container (EXEC-M11). Distroless + static binary.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/kanz-eng/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/webhook-ingest/Dockerfile .
#
# THE FIRST HOP OF THE TRADING LOOP. It had no image and no manifest, so nothing
# on this platform could accept a TradingView signal at all — the loop had no
# entry point in production (EXEC-M10, found by deploying to a real cluster).
FROM golang:1.26.1 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

# Dependency layer: go.mod/go.sum + the generated SDK the replace points at,
# so `go mod download` is cached across source-only changes.
COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
RUN go mod download

COPY kanz/ ./
# CGO off ⇒ a fully static binary, which is what distroless/static requires.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"       -o /out/webhook-ingest ./services/webhook-ingest/cmd/webhook-ingest

# Distroless static: no shell, no libc, no package manager. :nonroot runs as uid
# 65532, matching the pod's runAsUser.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/webhook-ingest /webhook-ingest
USER nonroot:nonroot
EXPOSE 8090
ENTRYPOINT ["/webhook-ingest"]
