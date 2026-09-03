# syntax=docker/dockerfile:1
#
# identity container. Distroless + static binary.
#
# The platform's own credential authority (#364): Argon2id credentials,
# single-use invitations, and the ES256 signer whose JWKS the gateway verifies
# against. It MINTS the tokens every other service's authority is expressed in.
#
# THIS SERVICE IS REACHABLE WITHOUT A TOKEN, deliberately and permanently. /login
# and /invites/redeem exist to be reached by people who hold none — so unlike
# every other upstream it cannot sit behind the NetworkPolicy that makes
# X-Kanz-Principal-* trustworthy, and it authenticates bearer tokens itself. That
# is a deployment consequence, not just a code one: whatever fronts this service
# is exposed to unauthenticated traffic by design, and its rate limiter
# (IDENTITY_LOGIN_BURST) is the only bound on credential guessing.
#
# The signing key is a MOUNTED SECRET (IDENTITY_SIGNING_KEY_FILE), never baked in
# and never generated per-pod in production: two replicas each generating their
# own key would serve two disjoint JWKS, so a token minted by one is refused by
# the other roughly half the time. IDENTITY_ALLOW_EPHEMERAL_KEY opts into that
# for a laptop and must not be set in a manifest running more than one replica.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/identity/Dockerfile .
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
# Argon2id here is golang.org/x/crypto/argon2, pure Go — no libargon2, so the
# credential path needs no C toolchain and the image needs no libc.
# VERSION is stamped into the binary (internal/version, #143). Unset here means
# the image reports "unstamped" rather than a plausible-looking "dev".
ARG VERSION=
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}" \
      -o /out/identity ./services/identity/cmd/identity

# Distroless static: no shell, no libc, no package manager — minimal attack
# surface, and worth more here than elsewhere because this image handles
# plaintext credentials on their way into Argon2id.
FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/identity /identity
USER nonroot:nonroot
EXPOSE 8087
ENTRYPOINT ["/identity"]
