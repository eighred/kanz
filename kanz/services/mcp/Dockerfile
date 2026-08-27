# syntax=docker/dockerfile:1
#
# mcp container (#743). Distroless + static binary.
#
# The agent-facing MCP READ plane: JSON-RPC over one HTTP route, exposing
# read-only tools over portfolio risk state to external agents, every one of them
# tenant-scoped through internal/agentgate.
#
# WHAT THIS IMAGE CANNOT DO. It links no exchange SDK, holds no venue credential,
# and imports neither the OMS order aggregate nor the execution router — asserted
# on the import graph by test/arch/mcp_is_a_read_plane_test.go rather than left
# to this comment. It speaks to exactly one upstream, the risk-engine's query.v1
# read API, and to no broker and no database at all.
#
# TWO PORTS, AND THE SPLIT IS A SECURITY BOUNDARY (#232/#409). :8110 carries the
# MCP route, which decides which TENANT's state an agent may read from the
# X-Kanz-Principal-* headers the api-gateway injects — so a NetworkPolicy admits
# only the gateway to it, and allow-observability-scrape deliberately does not.
# :8080 carries /metrics and nothing else, which is the port that rule admits.
# One mux for both is how every other header-trusting service on this platform
# ended up reachable from kanz-observability with a self-chosen principal.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). Build with:
#   docker build -f kanz/services/mcp/Dockerfile .
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
ARG VERSION=
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}" \
      -o /out/mcp ./services/mcp/cmd/mcp

# Distroless static: no shell, no libc, no package manager. :nonroot is uid 65532.
FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/mcp /mcp
USER nonroot:nonroot
EXPOSE 8110 8080
ENTRYPOINT ["/mcp"]
