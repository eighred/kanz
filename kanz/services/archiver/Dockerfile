# syntax=docker/dockerfile:1
#
# archiver container (DATA-M1). Distroless + static binary.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/archiver/Dockerfile .
FROM golang:1.26.5 AS build
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
      -o /out/archiver ./services/archiver/cmd/archiver

# Distroless static: no shell, no libc, no package manager — minimal attack
# surface. :nonroot runs as uid 65532, matching the pod's runAsUser.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/archiver /archiver
USER nonroot:nonroot
EXPOSE 8086
ENTRYPOINT ["/archiver"]
