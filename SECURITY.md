# Security policy

## Reporting a vulnerability

Report suspected vulnerabilities privately via a GitHub **Security Advisory**
("Report a vulnerability" on the repo Security tab) or to security@eighred.com.
Do not open a public issue for an unfixed vulnerability. Expect an
acknowledgement within **2 business days**.

## Vulnerability remediation SLA (SEC-02c)

Findings come from Dependabot (`.github/dependabot.yml`), the scheduled scan
(`.github/workflows/security.yml`: govulncheck / trivy / pip-audit / npm audit),
the per-release trivy gate (`release.yml`), and the post-deploy re-scan of the
SEC-02b vuln attestations. Time-to-remediate is measured from disclosure to a
fix merged to `main`:

| Severity (CVSS) | Remediate within | In-flight posture |
|---|---|---|
| Critical (9.0–10.0) | **3 days** | Block new releases of the affected component; hotfix + re-release. |
| High (7.0–8.9) | **14 days** | Prioritized over feature work; tracked to closure. |
| Medium (4.0–6.9) | **30 days** | Normal backlog; batched with routine bumps. |
| Low (< 4.0) | Best effort | Folded into the weekly Dependabot PRs. |

A finding with no upstream fix is risk-accepted with a documented justification
+ compensating control (e.g. the dep isn't on a reachable path), and re-reviewed
when a fix ships. `ignore-unfixed: true` in the scanners keeps the gates
actionable — they fail on what we can actually fix.

## Enforcement gates

These run automatically; an SLA breach is a process failure, not a tooling gap.

- **Build**: every release image is trivy-scanned and **fails on CRITICAL before
  signing** (`release.yml`); an unsigned image is refused at admission.
- **Deploy**: the cluster admission policy requires a cosign signature + SBOM +
  vuln-scan attestation (`infra/security/admission/`).
- **Source**: gitleaks blocks secrets in CI + a pre-commit hook
  (`kanz/.githooks/`, SEC-02d).
- **Dependencies**: Dependabot PRs + the scheduled scan (SEC-02c).

## Supported versions

`main` (and the most recent tagged release) receive security fixes.
