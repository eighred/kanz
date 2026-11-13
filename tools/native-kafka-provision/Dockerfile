FROM ghcr.io/eighred/base/golang:1.27.4@sha256:5e8afd493a7124a8586e134d9b532731161d4b9becf9dee5a5956b107426f93d AS build
WORKDIR /src
COPY kanz/go.mod kanz/go.sum ./kanz/
COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/build/go-mod-download.sh /usr/local/bin/go-mod-download.sh
WORKDIR /src/kanz
RUN sh /usr/local/bin/go-mod-download.sh
COPY kanz/ ./
ARG VERSION=
RUN test -n "$VERSION" && CGO_ENABLED=0 go test -c -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}" -o /probe ./cmd/kanz-kafka-provision
FROM ghcr.io/eighred/base/distroless-cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f
COPY --from=build /probe /probe
USER 65532:65532
ENTRYPOINT ["/probe", "-test.run=^TestNativeBrokerQualification$", "-test.v"]
