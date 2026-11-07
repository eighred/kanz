# syntax=docker/dockerfile:1
#
# lake-sink container: static Go owner plus optional confined GNU snapshot worker.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/lake-sink/Dockerfile .
FROM ghcr.io/eighred/base/golang:1.26.10 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

# Dependency layer: consumer and generated SDK module metadata only,
# so schema source changes preserve the verified dependency download layer.
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
# CGO off ⇒ a fully static binary (every dep — pgx, nats, kafka-go, go-spiffe —
# is pure Go), which is what distroless/static requires. -trimpath strips local
# paths; -s -w drop the symbol/debug tables.
# VERSION is stamped into the binary (internal/version, #143). Unset here means
# the image reports "unstamped" rather than a plausible-looking "dev".
ARG VERSION=
RUN CGO_ENABLED=0 go build -tags "${GOTAGS}" -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}" \
      -o /out/lake-sink ./services/lake-sink/cmd/lake-sink

# The optional worker is shipped with the existing owner; this does not enable
# snapshots or supply the operator-required expected executable digest.
FROM ghcr.io/eighred/base/rust:1.90.0-bookworm@sha256:3914072ca0c3b8aad871db9169a651ccfce30cf58303e5d6f2db16d1d8a7e58f AS rust-tools
RUN cargo install --locked --version 0.7.7 cargo-auditable
RUN cargo install --locked --version 0.1.78 cargo-chef
WORKDIR /src/kanz-rs

FROM rust-tools AS rust-planner
COPY kanz-rs/ ./
RUN cargo chef prepare --recipe-path /recipe.json

FROM rust-tools AS rust-build
# Only dependency manifests, lockfile and build configuration reach this layer.
# VERSION and real source enter afterward; dummy workspace artifacts are removed
# by chef before the original manifests and build scripts are restored.
COPY --from=rust-planner /recipe.json /recipe.json
RUN cargo auditable chef cook --locked --release --recipe-path /recipe.json -p kanz-lake-snapshot
COPY kanz-rs/ ./
COPY kanz-schemas/proto/ /src/kanz-schemas/proto/
ARG VERSION
RUN test -n "$VERSION" && KANZ_BUILD_VERSION="$VERSION" cargo auditable build --locked --release --manifest-path Cargo.toml -p kanz-lake-snapshot

# GNU worker needs the pinned C/C++ runtime; the Go owner remains fully static.
# Root-owned, nonwritable worker bytes are required by the verified-FD launcher.
FROM ghcr.io/eighred/base/distroless-cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f
COPY --from=build /out/lake-sink /lake-sink
COPY --from=rust-build --chown=0:0 --chmod=0555 /src/kanz-rs/target/release/kanz-lake-snapshot /kanz-lake-snapshot
USER nonroot:nonroot
EXPOSE 8085
ENTRYPOINT ["/lake-sink"]
