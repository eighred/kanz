# syntax=docker/dockerfile:1
#
# api-gateway container (CICD-01c). Distroless + static binary, mirroring the
# risk-engine image.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/api-gateway/Dockerfile .
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
# CGO off ⇒ a fully static binary (every dep is pure Go), which is what
# distroless/static requires. -trimpath strips local paths; -s -w drop the
# symbol/debug tables.
# VERSION is stamped into the binary (internal/version, #143). Unset here means
# the image reports "unstamped" rather than a plausible-looking "dev".
ARG VERSION=
# -tags redis LINKS THE CROSS-POD IDEMPOTENCY CLAIM (#721).
#
# Without it the only claim available to the binary is the per-pod one, which is correct for
# EXACTLY ONE REPLICA — and api-gateway-deploy.yaml runs replicas: 2. Two pods are two maps,
# so a client retry carrying the same Idempotency-Key that lands on the other pod is not
# recognised as a retry: on /v1/orders it mints a SECOND order id and publishes a SECOND
# SubmitOrder. Nothing downstream can collapse them, because the OMS's ON CONFLICT DO NOTHING
# keys on order_id and these are two different ones — one client intent, two live orders.
#
# The binary REFUSES TO START if API_GATEWAY_REDIS_URL is set without this tag, rather than
# degrading quietly to the per-pod claim. See cmd/api-gateway/idemclaims_noredis.go.
RUN CGO_ENABLED=0 go build -tags redis -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}" \
      -o /out/api-gateway ./services/api-gateway/cmd/api-gateway

# Distroless static: no shell, no libc, no package manager — minimal attack
# surface. :nonroot runs as uid 65532. (Pinning by digest is a CICD-01d
# supply-chain hardening follow-up.)
FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/api-gateway /api-gateway
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/api-gateway"]
