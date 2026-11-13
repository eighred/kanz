# syntax=docker/dockerfile:1
#
# kanz-kafka-provision: opt-in non-destructive tenant topic/ACL Job.
# No broker, PVC or default onboarding changes. Build context is the repo ROOT:
#   docker build -f kanz/cmd/kanz-kafka-provision/Dockerfile .
FROM ghcr.io/eighred/base/golang:1.27.4 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

COPY kanz-schemas/gen/go/go.mod kanz-schemas/gen/go/go.sum ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
# The download is retried (#776): the module proxy and checksum database drop
# HTTP/2 streams mid-transfer, and a bare `go mod download` turns that into a
# red build on a commit that is fine. The script is shared rather than copied
# into 29 Dockerfiles, and a genuine outage still fails this build.
COPY kanz/build/go-mod-download.sh /usr/local/bin/go-mod-download.sh
RUN sh /usr/local/bin/go-mod-download.sh

COPY kanz-schemas/gen/go/ /src/kanz-schemas/gen/go/
COPY kanz/ ./
# VERSION is stamped into the binary (internal/version, #143). Unset here means
# the image reports "unstamped" rather than a plausible-looking "dev".
ARG VERSION=
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}" \
      -o /out/kanz-kafka-provision ./cmd/kanz-kafka-provision

FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/kanz-kafka-provision /kanz-kafka-provision
USER nonroot:nonroot
ENTRYPOINT ["/kanz-kafka-provision"]
