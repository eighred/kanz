# Security gate self-tests (SEC-02e)

Proof that the SEC-02 gates actually catch what they claim — a gate that silently
passes everything is worse than none.

| Script | Asserts | Where it runs |
|---|---|---|
| `gitleaks-planted-secret.sh` | a planted (non-allowlisted) secret IS detected by gitleaks using the real repo config | `.github/workflows/security.yml` (`secret-scan-selftest`, every PR) |
| `verify-admission.sh <image@digest>` | a release image passes the cosign signature + SBOM + vuln-scan checks the cluster admission policy enforces; an unsigned/foreign image fails | local / CI-optional (needs cosign + registry) |

```sh
sh kanz/infra/security/test/gitleaks-planted-secret.sh
sh kanz/infra/security/test/verify-admission.sh ghcr.io/kanz-eng/risk-engine@sha256:<digest>
```

## Why these two

- **Secret scanning** (SEC-02d) is only as good as its config — an over-broad
  allowlist would pass real secrets. The planted-secret test fails the build if
  the gate ever stops detecting a canonical credential, so the allowlist can be
  tuned for false positives without fear of silently disabling the gate.
- **Admission** (SEC-02a/b) lives in the cluster webhook (`mode: enforce`,
  fail-closed). `verify-admission.sh` reproduces the exact cosign checks the
  webhook runs, so a developer can prove a release is admissible (or a tampered
  image isn't) without deploying. The negative case — unsigned ⇒ first verify
  fails ⇒ admission rejects — is the "critical-CVE image rejected" guarantee,
  since a critical CVE fails `release.yml`'s trivy gate before signing
  (SEC-02b).
