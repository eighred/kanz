# syntax=docker/dockerfile:1
#
# schema-registry container (CICD-01c). Distroless + static binary.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/kanz-eng/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/schema-registry/Dockerfile .
FROM golang:1.26.1 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
RUN go mod download

COPY kanz/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/schema-registry ./services/schema-registry/cmd/schema-registry

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/schema-registry /schema-registry
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/schema-registry"]
