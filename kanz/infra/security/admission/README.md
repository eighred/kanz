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

1. a cosign signature from the release-workflow identity above,
2. an SPDX SBOM **attestation** (not just a signature) — an image with no
   provenance record is rejected, and
3. a **vulnerability-scan attestation** (cosign-vuln predicate, SEC-02b) — an
   image that never ran a scan is rejected.

`mode: enforce` fails closed: if verification can't complete, admission is
denied rather than allowed.

## Critical-CVE blocking (SEC-02b)

The "no critical CVEs at deploy" guarantee is enforced **at the choke point that
mints the signature**: `release.yml` runs `trivy ... --severity CRITICAL
--exit-code 1` *before* the cosign sign/attest steps, so a critical-CVE image is
pushed but never signed — and requirement (1) above then refuses it at
admission. The vuln attestation (3) makes the scan result auditable on the image
and gives the **SEC-02c scheduled re-scan** a baseline to catch CVEs *disclosed
after* a release (the case an inline admission check can't see at sign time);
that re-scan alerts under the SECURITY.md remediation SLA rather than silently
admitting. This is defense-in-depth: signature gate (deploy) + continuous
re-scan (post-deploy), not a single fragile inline CVE parse.

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
