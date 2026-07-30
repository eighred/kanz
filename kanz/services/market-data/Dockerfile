# syntax=docker/dockerfile:1
#
# market-data container (CICD-01c). Distroless + static binary.
#
# The durable price-history service: folds market.v1 FACTs off the bus into the
# price_observations store the risk engine marks against. No build tags, no
# variants — it holds no credentials and links no vendor SDK.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/market-data/Dockerfile .
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
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/market-data ./services/market-data/cmd/market-data

# Distroless static: no shell, no libc, no package manager — minimal attack
# surface. :nonroot runs as uid 65532. (Pinning by digest is a CICD-01d
# supply-chain hardening follow-up.)
FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/market-data /market-data
USER nonroot:nonroot
EXPOSE 8082
ENTRYPOINT ["/market-data"]
