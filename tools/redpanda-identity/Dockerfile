FROM ghcr.io/eighred/base/golang:1.26.11@sha256:7af425da11f4fc1a9308eb704369e3f6d1c44e35bac0f874a59ade3f20fdc07e AS build
WORKDIR /src
COPY kanz/go.mod kanz/go.sum ./kanz/
COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/build/go-mod-download.sh /usr/local/bin/go-mod-download.sh
WORKDIR /src/kanz
RUN sh /usr/local/bin/go-mod-download.sh
# The probe exercises the real replay/rebuild owners and their bus dependency
# closure. Keep the module build context complete; only /probe enters runtime.
COPY kanz/ ./
ARG VERSION=
RUN test -n "$VERSION" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}" -o /probe ./tools/redpandaidentity
FROM ghcr.io/eighred/base/distroless-cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f
COPY --from=build /probe /probe
USER 65532:65532
ENTRYPOINT ["/probe"]
