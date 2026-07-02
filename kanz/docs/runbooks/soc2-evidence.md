---
title: SOC 2 Type II continuous evidence
severity: ticket
---

# SOC 2 Type II continuous evidence (PARITY-06d)

A SOC 2 **Type II** audit asks whether controls *operated throughout the period*,
not merely that they exist. The AUDIT-01 observation stream is an append-only,
hash-chained WORM log of exactly the operating events auditors sample — so the
evidence collects itself. The `soc2` package
(`services/audit/internal/soc2`) maps the Trust Services Criteria to the record
`Kind`s that evidence them and produces a per-control evidence bundle over any
window.

## Control mapping (what the stream evidences)

| Control | Category | Evidenced by (audit `Kind`) | Meaning |
|---|---|---|---|
| CC6.1 | Security | `authz_decision` | Every logical-access decision is recorded (AUTH-01d) |
| CC7.2 | Security | `data_quality` | Anomaly monitoring — DATA-05 quality events are detected + logged |
| CC8.1 | Security | `command`, `command_outcome` | Changes are authorized and their outcomes recorded |
| PI1.1 | Processing Integrity | `decision`, `command_outcome` | Processing is complete + accurate — model/strategy decisions logged |

These are the controls the stream can HONESTLY evidence. Controls that need
out-of-band artifacts — HR onboarding/offboarding, vendor security reviews,
physical/data-center access, pen-test cadence — are collected by the ops process
below, NOT fabricated from the stream. The mapping is data (`DefaultControls`); a
deployment extends it for its own control matrix.

## Pull the evidence bundle

```sh
# A calendar-quarter window; `to` defaults to now. 409 ⇒ a mapped control had
# insufficient evidence in the window (a Type II exception a monitor alerts on).
curl -fsS "$AUDIT/v1/soc2/evidence?from=2026-04-01T00:00:00Z&to=2026-07-01T00:00:00Z" | jq .
```

The bundle lists, per control, the supporting `count`, sample `event_id`s (the
records an auditor pulls to verify the chain via `/v1/audit/events/{id}` +
`/v1/audit/verify`), and `satisfied`; the top-level `gaps` array names any control
below its `min_per_window`.

## Continuous-evidence pipeline (the running Type II file)

`soc2.CollectFromStore` is the collection primitive. Run it on a schedule so the
evidence file is continuous, not reconstructed at audit time:

- **Nightly**: a cron/Job calls `/v1/soc2/evidence` for the trailing 24h and
  archives the JSON to the immutable evidence bucket (same retention plane as the
  DR backups). A non-200 pages the compliance on-call — a control that stopped
  producing evidence is a control that stopped operating.
- **Per-quarter**: the audit-period bundle is the artifact handed to the external
  auditor, alongside a `/v1/audit/verify` attestation proving the chain of the
  sampled records is intact (tamper-evidence, AUDIT-01b).

## Out-of-band controls (ops-owned, not stream-derived)

Collect these into the same evidence bucket on their own cadence, cross-referenced
by control id: access reviews (quarterly), vendor reviews (annual), pen-test +
remediation (annual, see `PARITY-06f`), change-approval records not routed through
the command bus, and BC/DR test results (the `dr-drill.md` quarterly output — RTO/
RPO evidence for the Availability criteria).

## Confirm the pipeline is healthy

```sh
# The evidence endpoint is reachable and the trailing-24h window is satisfied.
curl -fsS -o /dev/null -w '%{http_code}\n' "$AUDIT/v1/soc2/evidence?from=$(date -u -d '24 hours ago' +%FT%TZ)"
# 200 = all mapped controls produced evidence in the last day; 409 = a gap to chase.
```
