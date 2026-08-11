# syntax=docker/dockerfile:1
#
# optimization container (CICD-01c). Distroless + static binary.
#
# Portfolio construction: mean-variance / min-variance / max-Sharpe / risk parity,
# Black-Litterman and HRP, and the bridge that materializes an approved proposal
# into order.v1 commands.
#
# STATELESS, and unusually so for this platform: the market inputs (covariance,
# expected returns, current weights, prices) all arrive in the request, so there
# is no database, no broker subscription and no migration initContainer. It boots
# ready.
#
# TWO PORTS, AND THE SPLIT IS A SECURITY BOUNDARY (#409). :8100 carries the API,
# including the route that materializes a proposal into order commands and takes
# the issuer of those commands from the gateway-injected principal — so a
# NetworkPolicy admits only api-gateway to it. :8094 carries /metrics and nothing
# else, because allow-observability-scrape must admit whatever port serves
# telemetry, and one mux for both is how every other header-trusting service on
# this platform ended up reachable from kanz-observability (#232). The API port is
# 8100 rather than 8080 because that rule selects EVERY pod and admits 8080.
#
# No build tags and no variants: it holds no exchange credentials and links no
# vendor SDK.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/optimization/Dockerfile .
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
# -trimpath strips local paths; -s -w drop the symbol/debug tables.
# VERSION is stamped into the binary (internal/version, #143). Unset here means
# the image reports "unstamped" rather than a plausible-looking "dev".
ARG VERSION=
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}" \
      -o /out/optimization ./services/optimization/cmd/optimization

# Distroless static: no shell, no libc, no package manager — minimal attack
# surface. :nonroot runs as uid 65532.
FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/optimization /optimization
USER nonroot:nonroot
EXPOSE 8100 8094
ENTRYPOINT ["/optimization"]
