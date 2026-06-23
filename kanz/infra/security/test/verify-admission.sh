#!/bin/sh
# SEC-02e: exercise the admission signature/attestation gate
# (infra/security/admission/cluster-image-policy.yaml) WITHOUT a cluster. cosign
# verify reproduces exactly what the sigstore policy-controller webhook checks,
# so this is the local/CI-optional proof of the same logic:
#
#   - a signed release image (built by release.yml on a vN tag) PASSES all three
#     checks below;
#   - an unsigned or foreign image FAILS the first `cosign verify` (non-zero),
#     which is exactly the admission rejection.
#
# Needs cosign + registry network access; run against a published digest:
#   sh verify-admission.sh ghcr.io/kanz-eng/risk-engine@sha256:<digest>
#
# The live enforcement is the cluster webhook (mode: enforce, fail-closed); this
# is the developer/CI smoke that the policy identity + required attestations are
# what the release workflow actually produces.
set -eu

IMAGE="${1:?usage: verify-admission.sh <image@sha256:digest>}"
ISSUER="https://token.actions.githubusercontent.com"
IDENTITY='^https://github.com/kanz-eng/kanz/.github/workflows/release.yml@refs/tags/v.*'

if ! command -v cosign >/dev/null 2>&1; then
  echo "SKIP: cosign not installed (https://docs.sigstore.dev/cosign/installation)" >&2
  exit 0
fi

echo "1/3 signature ..."
cosign verify "$IMAGE" \
  --certificate-oidc-issuer "$ISSUER" --certificate-identity-regexp "$IDENTITY" >/dev/null

echo "2/3 SBOM attestation ..."
cosign verify-attestation "$IMAGE" --type spdxjson \
  --certificate-oidc-issuer "$ISSUER" --certificate-identity-regexp "$IDENTITY" >/dev/null

echo "3/3 vuln-scan attestation ..."
cosign verify-attestation "$IMAGE" --type vuln \
  --certificate-oidc-issuer "$ISSUER" --certificate-identity-regexp "$IDENTITY" >/dev/null

echo "OK: $IMAGE is signed + SBOM/vuln-attested by the release workflow — admission would ADMIT it."
echo "    (an unsigned/foreign image fails step 1 — admission would REJECT it.)"
