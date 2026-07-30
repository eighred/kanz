# syntax=docker/dockerfile:1
#
# kanz-provisioner: the one-shot node-join Job (S2a). The SOLE binary importing
# crypto/ssh. Distroless static. Build context is the repo ROOT:
#   docker build -f kanz/cmd/kanz-provisioner/Dockerfile .
FROM ghcr.io/eighred/base/golang:1.26.5 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
RUN go mod download

COPY kanz/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/kanz-provisioner ./cmd/kanz-provisioner

FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/kanz-provisioner /kanz-provisioner
USER nonroot:nonroot
ENTRYPOINT ["/kanz-provisioner"]
