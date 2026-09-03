# syntax=docker/dockerfile:1
#
# market-ingest container (INFRA-M7a-4). Distroless + static binary.
#
# No GOTAGS build-arg, and no variant of this image. The exchange feeds used to be
# build-tag split, and an untagged build fell back to publishing GENERATED prices
# onto the bus as market.v1 FACTs while reporting itself healthy. Those tags are
# gone: the Binance and OKX feeds compile in unconditionally (they are public
# market data over a plain websocket — no API key, no vendor SDK), and WHICH ones
# run is decided at runtime by the symbol maps, as it always was.
#
# So there is one image, and what it ingests is a line of deployment config you can
# read rather than a build arg you cannot.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). Build with:
#   docker build -f kanz/services/market-ingest/Dockerfile .
FROM ghcr.io/eighred/base/golang:1.26.5 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
# The download is retried (#776): the module proxy and checksum database drop
# HTTP/2 streams mid-transfer, and a bare `go mod download` turns that into a
# red build on a commit that is fine. The script is shared rather than copied
# into 29 Dockerfiles, and a genuine outage still fails this build.
COPY kanz/build/go-mod-download.sh /usr/local/bin/go-mod-download.sh
RUN sh /usr/local/bin/go-mod-download.sh

COPY kanz/ ./
# CGO off ⇒ a fully static binary, which is what distroless/static requires.
# VERSION is stamped into the binary (internal/version, #143). Unset here means
# the image reports "unstamped" rather than a plausible-looking "dev".
ARG VERSION=
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}" \
      -o /out/market-ingest ./services/market-ingest/cmd/market-ingest

# Distroless static: no shell, no libc, no package manager. This process holds no
# credentials at all — it reads PUBLIC quotes — which is exactly why it did not
# need the process isolation the venue adapters got.
FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/market-ingest /market-ingest
USER nonroot:nonroot
EXPOSE 8091
ENTRYPOINT ["/market-ingest"]
