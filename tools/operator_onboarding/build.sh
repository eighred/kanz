#!/usr/bin/env bash
set -euo pipefail

# Package the reviewed programs and public manifests. No runtime credential
# exists in this job. The proof job receives this same-run artifact by name.
out=$(mktemp -d "${RUNNER_TEMP:?}/kanz77-build.XXXXXXXX")
context="$out/context"
mkdir "$context" "$out/input"
(
  cd kanz
  while read -r binary package; do
    CGO_ENABLED=0 go build -trimpath -o "$context/$binary" "$package"
  done <<'PROGRAMS'
operator ./services/operator/cmd/operator
gateway ./services/api_gateway/cmd/api_gateway
web-bff ./services/web_bff/cmd/web_bff
provisioner ./cmd/kanz_provisioner
identity ./services/identity/cmd/identity
invite ./cmd/kanz_invite
migrate ./cmd/kanz_migrate
PROGRAMS
)
cp -R web/dist "$context/static"
mkdir -p "$context/migrations" "$context/etc/ssl/certs"
cp -R kanz/services/identity/migrations "$context/migrations/identity"
cp /etc/ssl/certs/ca-certificates.crt "$context/etc/ssl/certs/"
cat > "$context/Dockerfile" <<'IMAGE'
FROM scratch
COPY . /
USER 65532:65532
ENTRYPOINT ["/provisioner"]
IMAGE
image="docker.io/kanz-fixture/onboarding:$GITHUB_SHA"
docker build -t "$image" "$context"
docker save -o "$out/input/onboarding.tar" "$image"
git rev-parse HEAD > "$out/input/source-sha"
test "$(cat "$out/input/source-sha")" = "$GITHUB_SHA"
git archive --format=tar "$GITHUB_SHA" tools/operator_onboarding tools/feed_identity/proof.py \
  kanz/infra/security/spire kanz/infra/deploy/operator-deploy.yaml kanz/infra/deploy/web-bff-deploy.yaml \
  kanz/infra/deploy/api-gateway-deploy.yaml web/package.json web/package-lock.json web/.nvmrc |
  tar -x -C "$out/input"
tar -C "$out/input" -czf "$RUNNER_TEMP/operator-onboarding.tgz" .
