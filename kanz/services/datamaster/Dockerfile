# syntax=docker/dockerfile:1
#
# datamaster container (CICD-01c). Distroless + static binary.
#
# the golden-source data master. NOTE: its store (MemoryGoldenStore /
# PostgresGolden) is still ORPHANED — server.New takes no store at all, so nothing
# consumes it. This image makes the service deployable, not yet useful; giving it a
# durable store means first designing what reads a golden store.
#
# No build tags and no variants: it holds no exchange credentials and links no
# vendor SDK. What it stores is a deployment decision (DATAMASTER_DATABASE_URL_FILE), not a
# compile-time one.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/kanz-eng/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/datamaster/Dockerfile .
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
# CGO off ⇒ a fully static binary (every dep — pgx, nats, kafka-go, go-spiffe —
# is pure Go), which is what distroless/static requires. -trimpath strips local
# paths; -s -w drop the symbol/debug tables.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/datamaster ./services/datamaster/cmd/datamaster

# Distroless static: no shell, no libc, no package manager — minimal attack
# surface. :nonroot runs as uid 65532. (Pinning by digest is a CICD-01d
# supply-chain hardening follow-up.)
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/datamaster /datamaster
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/datamaster"]
