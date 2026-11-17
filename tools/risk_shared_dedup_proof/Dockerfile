# syntax=docker/dockerfile:1
FROM ghcr.io/eighred/base/golang:1.27.4@sha256:5e8afd493a7124a8586e134d9b532731161d4b9becf9dee5a5956b107426f93d AS tools
ENV GOTOOLCHAIN=local GOFLAGS=-p=2 CGO_ENABLED=0
RUN go install github.com/bufbuild/buf/cmd/buf@v1.69.0 && \
    go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11 && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
COPY kanz/go.mod /src/kanz/go.mod
COPY schemas/ /src/schemas/
WORKDIR /src/schemas
# Generate the canonical Go projection. The Python and TypeScript plugin
# outputs are not inputs to either service image or the probe.
RUN grep -qx '  # Python:.*' buf.gen.yaml && \
    sed '/^  # Python:/,$d' buf.gen.yaml > /tmp/go.yaml && \
    test "$(grep -c '^  - local:' /tmp/go.yaml)" -eq 2 && \
    ! grep -q 'remote:' /tmp/go.yaml && \
    buf generate --template /tmp/go.yaml && bash bootstrap-go-sdk.sh

FROM scratch AS sdk
COPY --from=tools /src/schemas/gen/go/ /

FROM tools AS compile-driver
WORKDIR /src/kanz
COPY kanz/go.mod kanz/go.sum ./
RUN go mod download
COPY kanz/ ./
RUN --network=none go test -mod=readonly -c -trimpath -buildvcs=false -o /out/driver ./test/risk_shared_dedup_proof

FROM scratch AS driver
COPY --from=compile-driver /out/driver /driver
USER 65534:65534
ENTRYPOINT ["/driver"]
