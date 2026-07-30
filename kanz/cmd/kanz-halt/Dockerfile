# syntax=docker/dockerfile:1
#
# kanz-halt container (SEC-M3c). Distroless + static binary.
#
# Build context is the repo ROOT, not kanz/ — same as every other image here: the
# module's go.mod replaces github.com/eighred/kanz/kanz-schemas-go =>
# ../kanz-schemas/gen/go, which is generated-not-committed (EVT-15a). Build with:
#   docker build -f kanz/cmd/kanz-halt/Dockerfile .
#
# # Why the break-glass tool needs an image at all
#
# It did not have one, and that was fine while the broker took plaintext: an
# operator ran the binary from a laptop. The production broker does not — it
# requires an SVID (nats.yaml `tls { verify: true, verify_and_map: true }`), and
# SPIRE issues an SVID to a POD, from its ServiceAccount. So the operator plane
# runs IN the cluster now (the 2026-07-16 decision; see KANZ_BRAIN.md), and a pod
# needs an image.
#
# Consequence to hold onto, because it is the cost of that decision: the brake is
# reachable only through a cluster healthy enough to schedule a Job. That is an
# argument for a break-glass path that does not schedule — not an argument for
# leaving the brake unable to authenticate.
#
# # It stays STANDALONE
#
# kanz-halt shares no code with the trading pipeline: it imports pkg/bus, the
# generated schemas, a constants-only subject package, and now pkg/transport (the
# identity layer — not the pipeline). The one tool that must work while the system
# is on fire is not coupled to the system that is on fire, and this image carries
# nothing but it.
FROM ghcr.io/eighred/base/golang:1.26.5 AS build
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
      -o /out/kanz-halt ./cmd/kanz-halt

# Distroless static: no shell, no libc, no package manager. :nonroot runs as uid
# 65532, matching the Job's runAsUser.
FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/kanz-halt /kanz-halt
USER nonroot:nonroot
ENTRYPOINT ["/kanz-halt"]
