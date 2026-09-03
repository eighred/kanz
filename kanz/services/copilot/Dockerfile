# syntax=docker/dockerfile:1
#
# copilot container (CICD-01c). Distroless + static binary.
#
# BUILT WITH `-tags anthropic,openrouter` ON PURPOSE.
#
# The LLM adapter is build-tag split. WITHOUT the tag, no Claude client is linked
# and the binary serves llm.StubModel — which does not answer a portfolio
# manager's questions, it FABRICATES answers, and they read exactly like real
# ones. There is no dashboard on which that looks wrong.
#
# That is the same defect as an OMS routing to SimVenue or a market-ingest
# publishing SimFeed prices, and it is the same rule broken: never inject data. So
# the shipped image links the real model, and an untagged build now REFUSES to
# start unless COPILOT_ALLOW_STUB=true is set deliberately.
#
# The tag exists to keep anthropic-sdk-go out of builds that do not need it — not
# to make fabrication a default.
#
# Build context is the repo ROOT, not kanz/: the module's go.mod replaces
# github.com/eighred/kanz/kanz-schemas-go => ../kanz-schemas/gen/go, which is
# generated-not-committed (EVT-15a). The build workflow runs `buf generate`
# first, so both trees sit side by side in the context. Build with:
#   docker build -f kanz/services/copilot/Dockerfile .
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
# CGO off ⇒ a fully static binary (every dep — pgx, nats, kafka-go, go-spiffe —
# is pure Go), which is what distroless/static requires. -trimpath strips local
# paths; -s -w drop the symbol/debug tables.
# BOTH PROVIDERS ARE LINKED; COPILOT_PROVIDER PICKS ONE AT RUNTIME (#179).
# Linking is a build decision, selection is a deployment one — an image that
# linked only what its author happened to want would make provider choice
# somebody else's, which is exactly what that change removed.
# VERSION is stamped into the binary (internal/version, #143). Unset here means
# the image reports "unstamped" rather than a plausible-looking "dev".
ARG VERSION=
RUN CGO_ENABLED=0 go build -tags anthropic,openrouter -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped=${VERSION}" \
      -o /out/copilot ./services/copilot/cmd/copilot

# Distroless static: no shell, no libc, no package manager — minimal attack
# surface. :nonroot runs as uid 65532. (Pinning by digest is a CICD-01d
# supply-chain hardening follow-up.)
FROM ghcr.io/eighred/base/distroless-static:nonroot
COPY --from=build /out/copilot /copilot
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/copilot"]
