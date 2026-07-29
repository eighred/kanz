# syntax=docker/dockerfile:1
#
# venue-binance container (INFRA-M7a-2). Distroless + static binary.
#
# Note what is NOT here: a GOTAGS build-arg. The OMS image needs one because its
# venue connectors are build-tag split and an untagged build is a simulator. This
# image IS the Binance connector — there is nothing to switch on, and no variant
# of it that quietly trades against nothing.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). Build with:
#   docker build -f kanz/services/venue-binance/Dockerfile .
FROM golang:1.26.5 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
RUN go mod download

COPY kanz/ ./
# CGO off ⇒ a fully static binary, which is what distroless/static requires.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/venue-binance ./services/venue-binance/cmd/venue-binance

# Distroless static: no shell, no libc, no package manager. This process holds
# exchange API credentials and can move money — the attack surface is the point.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/venue-binance /venue-binance
USER nonroot:nonroot
EXPOSE 9000 8091
ENTRYPOINT ["/venue-binance"]
