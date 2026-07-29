# syntax=docker/dockerfile:1
#
# operator service (F0+S1). Distroless + static binary. Build context is the
# repo ROOT, not kanz/ — the module replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, generated
# (not committed). Build with:
#   docker build -f kanz/services/operator/Dockerfile .
FROM golang:1.26.5 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
RUN go mod download

COPY kanz/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/operator ./services/operator/cmd/operator

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/operator /operator
USER nonroot:nonroot
ENTRYPOINT ["/operator"]
