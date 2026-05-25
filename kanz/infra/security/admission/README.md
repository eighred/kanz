# Image admission verification (CICD-01d)

The cluster-side half of the supply-chain gate. The release workflow
(`.github/workflows/release.yml`) builds, scans (trivy, fail-on-CRITICAL),
SBOM-attests (syft), and **keyless-signs** (cosign → Fulcio/Rekor) each service
image. This policy makes the cluster **refuse to run anything that isn't signed
by that workflow** — closing the loop so a tampered or unsigned image can't be
deployed even if it reaches the registry.

## Why keyless

No signing key exists to store, leak, or rotate. The signature carries a
short-lived Fulcio certificate bound to the **workflow's OIDC identity**
(`https://github.com/kanz-eng/kanz/.github/workflows/release.yml@refs/tags/v*`),
recorded in the Rekor transparency log. Verification pins both the issuer
(GitHub Actions) and that exact subject — the same "identity, not a stored
secret" stance as SEC-01 (SPIRE SVIDs, Vault SPIFFE auth).

## What's enforced

`cluster-image-policy.yaml` (`ClusterImagePolicy`) requires, for every
`ghcr.io/kanz-eng/**` image:

1. a cosign signature from the release-workflow identity above, and
2. an SPDX SBOM **attestation** (not just a signature) — an image with no
   provenance record is rejected.

`mode: enforce` fails closed: if verification can't complete, admission is
denied rather than allowed.

## Deploy

Requires [sigstore policy-controller](https://docs.sigstore.dev/policy-controller/overview/)
(admission webhook) in the cluster — install once from the pinned chart, then:

```sh
kubectl label namespace kanz-services policy.sigstore.dev/include=true
kubectl apply -f cluster-image-policy.yaml
```

The webhook only evaluates pods in namespaces carrying the
`policy.sigstore.dev/include: "true"` label.

## Verify a release locally

```sh
cosign verify ghcr.io/kanz-eng/risk-engine@<digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/kanz-eng/kanz/.github/workflows/release.yml@refs/tags/v.*'
```

## Follow-ups

- Pin the distroless base + all actions/tools by digest (started here; the
  Dockerfiles still pin the base by tag).
- Kyverno alternative if the cluster standardizes on it instead of
  policy-controller — the same keyless identity check expresses cleanly there too.
