# syntax=docker/dockerfile:1
#
# webhook-ingest container (EXEC-M11). Distroless + static binary.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/webhook-ingest/Dockerfile .
#
# THE FIRST HOP OF THE TRADING LOOP. It had no image and no manifest, so nothing
# on this platform could accept a TradingView signal at all — the loop had no
# entry point in production (EXEC-M10, found by deploying to a real cluster).
FROM ghcr.io/eighred/base/golang:1.26.5 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

# Dependency layer: go.mod/go.sum + the generated SDK the replace points at,
# so `go mod download` is cached across source-only changes.
COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
# The download is retried (#776): the module proxy and checksum database drop
# HTTP/2 streams mid-transfer, and a bare `go mod download` turns that into a
# red build on a commit that is fine. The script is shared rather than copied
# into 29 Dockerfiles, and a genuine outage still fails this build.
COPY kanz/build/go-mod-download.sh /usr/local/bin/go-mod-download.sh
RUN sh /usr/local/bin/go-mod-download.sh

COPY kanz/ ./
# CGO off ⇒ a fully static binary, which is what distroless/static requires.
# -tags redis (EXEC-M22) LINKS THE CROSS-POD NONCE STORE.
#
# Without it the only replay defence available to the binary is the in-process one, which is
# correct for EXACTLY ONE REPLICA: two pods are two maps, so a re-delivered TradingView alert
# landing on the other pod is admitted a SECOND time and fans out a SECOND set of orders
# against a live exchange. Nothing downstream catches it — a fresh claim mints a fresh
# signal_id, so the OMS's admission gate sees two different orders, not a duplicate.
#
# That pinned the platform's ENTRANCE to one pod on `Recreate`, which made every deploy,
# drain and eviction a window in which TradingView alerts hit nothing and were never retried.
#
# The tag and the manifest move TOGETHER: this binary refuses to start if
# WEBHOOK_INGEST_REDIS_URL is unset, and the untagged one refuses if it is SET — so the image
# and the deployment cannot silently disagree about which defence is in force.
# VERSION is stamped into the binary (internal/version, #143). Unset here means
# the image reports "unstamped" rather than a plausible-looking "dev".
ARG VERSION=
RUN CGO_ENABLED=0 go build -tags redis -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}"       -o /out/webhook-ingest ./services/webhook-ingest/cmd/webhook-ingest

# Distroless static: no shell, no libc, no package manager. :nonroot runs as uid
# 65532, matching the pod's runAsUser.
FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/webhook-ingest /webhook-ingest
USER nonroot:nonroot
EXPOSE 8090
ENTRYPOINT ["/webhook-ingest"]
