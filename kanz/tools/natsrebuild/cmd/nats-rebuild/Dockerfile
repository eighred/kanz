# syntax=docker/dockerfile:1
#
# nats-rebuild container (DR-01c). Distroless + static binary.
#
# Build context is the repo ROOT, not kanz/ — same as every other image here:
# the module's go.mod replaces github.com/eighred/kanz/kanz-schemas-go =>
# ../kanz-schemas/gen/go, which is generated-not-committed (EVT-15a). Build with:
#   docker build -f kanz/tools/natsrebuild/cmd/nats-rebuild/Dockerfile .
#
# # Why the DR tool needs an image
#
# It never had one, and that was invisible because nothing had run a failover.
# infra/dr/nats/rebuild-job.yaml has referenced ghcr.io/eighred/nats-rebuild
# since it was written; the image was in neither build.yml's nor release.yml's
# matrix, so it has never been published. infra/dr/failover.sh waits 600s on a
# Job that ImagePullBackOffs — the DR cutover could not complete.
#
# The Job also needs an IDENTITY, not just a binary: it publishes to a
# verify:true NATS cluster over mTLS, and SPIRE issues an SVID to a POD from
# its ServiceAccount. Same reason kanz-halt needs an image (SEC-M3c).
FROM golang:1.26.5 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
RUN go mod download

COPY kanz/ ./
# CGO off ⇒ a fully static binary, which is what distroless/static requires.
# -trimpath strips local paths; -s -w drop the symbol/debug tables.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/nats-rebuild ./tools/natsrebuild/cmd/nats-rebuild

# Distroless static: no shell, no libc, no package manager. :nonroot runs as uid
# 65532.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/nats-rebuild /nats-rebuild
USER nonroot:nonroot
ENTRYPOINT ["/nats-rebuild"]
