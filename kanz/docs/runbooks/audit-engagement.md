---
title: External audit & regulator engagement cycle
severity: ticket
---

# External audit & regulator engagement cycle (PARITY-06f)

The certification gate: turn the platform's internally-generated evidence (signed
filings, the tamper-evident audit chain, the SOC 2 continuous-evidence bundle)
into a completed third-party review and a first-client sign-off. This is the
process wrapper around the machine-produced artifacts of PARITY-06a–e — it does
not compute anything, it drives the human loop that certification requires.

## Engagement calendar

| Engagement | Cadence | Artifacts handed over | Owner |
|---|---|---|---|
| SOC 2 Type II audit | Annual (with a 6–12 month observation window) | `/v1/soc2/evidence` quarterly bundles + `/v1/audit/verify` attestations + out-of-band controls | Compliance |
| Penetration test | Annual + on major arch change | Scope, findings, remediation evidence | Security |
| Regulator filing review | Per filing cycle (FRTB/Form PF/AIFMD/TCFD/SFDR) | Signed `RegReport`s + the reconciliation worksheets (`FRTBResult`, `AIFMDResult`) | Regulatory |
| BC/DR attestation | Quarterly | `dr-drill.md` measured RPO/RTO | Platform |

Every artifact above is machine-generated and signed; the engagement is the act
of packaging, submitting, and closing findings — not re-deriving numbers.

## Findings remediation backlog

Every finding — from the auditor, the pen-tester, or the regulator — enters ONE
register (`findings-register.md`) and becomes a tracked board task. The register
is the single source of truth for "what is open against us."

Severity → SLA (time-to-remediate, from acceptance):

| Severity | SLA | Examples |
|---|---|---|
| Critical | 7 days | A cross-tenant leak, a broken audit chain, an unsigned filing path |
| High | 30 days | A control with recurring evidence gaps, a missing reconciliation |
| Medium | 90 days | A hardening gap, an incomplete runbook |
| Low | Next release | Cosmetic, documentation |

Lifecycle: `open → accepted → in-remediation → fixed → verified → closed`. A
finding is **verified** only by re-running the evidence that would have caught it
(the same discipline as a GameDay verify abort): a cross-tenant finding is closed
by the `tenant-onboarding.md` verify step passing; a filing finding by the
`Reconcile` gate passing against the regulator's worked example; a chain finding
by `/v1/audit/verify` returning 200. No finding closes on assertion alone.

## First-client UAT sign-off

The gate that everything above is true and stable for one real tenant. Run
against a production-shaped environment with the client's own (or representative)
data:

- [ ] Tenant provisioned via `tenant-onboarding.md`; the isolation `verify` step passed.
- [ ] Each capability the client contracted for returns correct results on their book (risk, IBOR NAV incl. multi-currency, the relevant regulatory filing).
- [ ] A signed regulatory filing reconciles against the client's independently-computed benchmark within tolerance (`Report.Reconcile`).
- [ ] The audit chain verifies (`/v1/audit/verify` → 200) over the UAT window, and the SOC 2 bundle for the window is `satisfied`.
- [ ] A DR drill (or the last quarterly drill's evidence) shows RPO/RTO within objective.
- [ ] Every finding raised during UAT is at `verified`/`closed` or has an accepted, SLA'd remediation plan the client signed off on.
- [ ] Sign-off recorded (signed) as an audit event — the UAT completion is itself evidence.

A UAT with an open Critical or High finding does **not** sign off; it converts to
a remediation sprint against the register and re-runs the affected checklist items.

## Closing the loop

Findings feed the backlog (`/forge-tasks` turns accepted findings into board
tasks); remediations land as normal changes gated by CI + the arch test; the fix
is verified by re-running the evidence. The register's "open Critical/High count"
is the headline certification-readiness metric — zero is the bar for go-live.
