# syntax=docker/dockerfile:1
#
# web-bff container. Distroless + static binary.
#
# The backend for frontend the web application talks to (#371). It holds the
# server-side session and proxies /api/* to the gateway's /v1/* as the session's
# caller.
#
# THE POINT OF THIS PROCESS IS THAT THE TOKEN NEVER LEAVES IT. The browser holds
# an opaque, httpOnly session id and nothing else; the ES256 token identity mints
# stays here. A token on this platform carries kanz-trader (moves capital) or
# kanz-operator (writes venue keys, drains nodes), so a token reachable from JS
# turns one XSS anywhere in the app into a silent, full-authority credential
# theft. That property is the reason this service exists rather than the SPA
# calling the gateway directly.
#
# It follows that the SESSION STORE is not an implementation detail of a single
# pod: sessions live server-side, so more than one replica needs a shared store
# or a user's next request lands on a pod that has never heard of them.
#
# Authority remains the GATEWAY's. This proxies; it does not decide. The
# capability mux (authz.Read/Trade/Operate) is the only authority, and a screen
# the SPA renders is refused there if the session's token does not permit it.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/web-bff/Dockerfile .
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
# CGO off => a fully static binary, which is what distroless/static requires.
# VERSION is stamped into the binary (internal/version, #143). Unset here means
# the image reports "unstamped" rather than a plausible-looking "dev".
ARG VERSION=
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}" \
      -o /out/web-bff ./services/web-bff/cmd/web-bff

# Distroless static: no shell, no libc, no package manager. This image holds live
# session tokens in memory, so the absence of a shell is doing real work here.
FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/web-bff /web-bff

# THE COMPILED SPA SHIPS IN THIS IMAGE, and that is a security property rather
# than packaging convenience.
#
# The BFF serves the application on its OWN origin and proxies /api/* from the
# same hostname, so the session cookie stays httpOnly + SameSite=Lax. Splitting
# the SPA onto a static host would make it a second origin, which forces
# SameSite=None — the setting that makes a cookie usable cross-site, and
# therefore the one CSRF defences exist to prevent — plus CORS-with-credentials
# and a cookie domain kept in step with whatever the edge is called this week.
#
# BUILT BY THE WORKFLOW, NOT HERE. kanz-web/dist is gitignored (it is a build
# artifact), and the build/release workflows run `npm ci && npm run build` into
# the docker context before this file is read — the same arrangement the
# generated Go SDK above uses, and for the same reason: no Node base image is
# mirrored under ghcr.io/eighred/base, and mirroring one to compile TypeScript
# in a container that ships neither is a lot of supply chain for no gain.
#
# WITHOUT THIS COPY the image builds, starts, serves /api and /auth perfectly,
# and returns nothing at / — an application that is finished, green, and blank
# in a browser. WEB_BFF_STATIC_DIR must point here;
# test/arch/bff_image_serves_the_spa_test.go fails the build if a manifest sets
# it to a path this file does not populate.
COPY kanz-web/dist/ /srv/spa/

USER nonroot:nonroot
EXPOSE 8084
ENTRYPOINT ["/web-bff"]
