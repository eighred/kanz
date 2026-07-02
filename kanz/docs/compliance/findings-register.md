# Findings register (PARITY-06f)

The single source of truth for every open finding against the platform — from
external auditors, penetration testers, regulators, and first-client UAT. Managed
per `docs/runbooks/audit-engagement.md`. Every row is also a tracked board task;
this register is the compliance-facing view of them.

**Certification-readiness metric:** open Critical + High count. Go-live bar = 0.

## Lifecycle

`open → accepted → in-remediation → fixed → verified → closed`

A finding is **verified** only by re-running the evidence that would have caught
it (never by assertion): cross-tenant → `tenant-onboarding.md` verify passes;
filing → `Report.Reconcile` passes vs the regulator worked example; chain →
`/v1/audit/verify` returns 200; SOC 2 control gap → `/v1/soc2/evidence` window
`satisfied`.

## SLA (from acceptance)

| Severity | SLA |
|---|---|
| Critical | 7 days |
| High | 30 days |
| Medium | 90 days |
| Low | next release |

## Register

| ID | Source | Opened | Severity | Control / area | Summary | Owner | Status | Verify evidence | Due | Closed |
|---|---|---|---|---|---|---|---|---|---|---|
| _example_ | SOC2 auditor | 2026-07-01 | High | CC7.2 | Data-quality monitoring had an evidence gap in May | platform | verified | `/v1/soc2/evidence` May window now `satisfied` | 2026-07-31 | 2026-07-20 |

<!-- Add one row per finding. Keep IDs stable (e.g. F-2026-001). Do not delete a
     closed row — the closed history IS audit evidence; move nothing out of the
     register. -->
