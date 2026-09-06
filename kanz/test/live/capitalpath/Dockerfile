# syntax=docker/dockerfile:1
# One-shot Issue #71 capital-path certifier. It contains no exchange credential:
# those terminate in the venue adapter. The Job receives only an authenticated
# gateway token, a SELECT-only ledger DSN, and a SPIFFE workload identity.
FROM ghcr.io/eighred/base/golang:1.26.5 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
COPY kanz/build/go-mod-download.sh /usr/local/bin/go-mod-download.sh
RUN sh /usr/local/bin/go-mod-download.sh

COPY kanz/ ./
ARG VERSION=
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}" \
      -o /out/kanz-capitalpath ./test/live/capitalpath

FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/kanz-capitalpath /kanz-capitalpath
USER nonroot:nonroot
ENTRYPOINT ["/kanz-capitalpath"]
