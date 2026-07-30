# syntax=docker/dockerfile:1
#
# venue-okx container (INFRA-M7a-3). Distroless + static binary.
#
# Note what is NOT here: a GOTAGS build-arg. There is nothing to switch on — this
# image IS the OKX connector, and there is no variant of it that quietly trades
# against nothing. (The OMS image no longer has one either: with OKX extracted,
# the last venue build tag is gone from the platform.)
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). Build with:
#   docker build -f kanz/services/venue-okx/Dockerfile .
FROM ghcr.io/eighred/base/golang:1.26.5 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
RUN go mod download

COPY kanz/ ./
# CGO off ⇒ a fully static binary, which is what distroless/static requires.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/venue-okx ./services/venue-okx/cmd/venue-okx

# Distroless static: no shell, no libc, no package manager. This process holds
# exchange API credentials and can move money — the attack surface is the point.
FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/venue-okx /venue-okx
USER nonroot:nonroot
EXPOSE 9000 8092
ENTRYPOINT ["/venue-okx"]
