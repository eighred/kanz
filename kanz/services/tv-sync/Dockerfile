# syntax=docker/dockerfile:1
#
# tv-sync container (EXEC-M11). Distroless + static binary.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/tv-sync/Dockerfile .
#
# The last hop: the Broker-API feedback projection TradingView reads. Like
# webhook-ingest, it had neither an image nor a manifest.
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
# CGO off ⇒ a fully static binary, which is what distroless/static requires.
# VERSION is stamped into the binary (internal/version, #143). Unset here means
# the image reports "unstamped" rather than a plausible-looking "dev".
ARG VERSION=
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}"       -o /out/tv-sync ./services/tv-sync/cmd/tv-sync

# Distroless static: no shell, no libc, no package manager. :nonroot runs as uid
# 65532, matching the pod's runAsUser.
FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/tv-sync /tv-sync
USER nonroot:nonroot
EXPOSE 8091
ENTRYPOINT ["/tv-sync"]
