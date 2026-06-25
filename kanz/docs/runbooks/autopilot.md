---
title: autopilot autonomous operations
alert: AutopilotEscalation
severity: page
slo: autopilot/control-loop
---

# autopilot autonomous operations (AUTO-01)

The `autopilot` service is the closed-loop ops controller: it watches the
operational signal stream and runs **runbook-as-code** — the human runbooks in
this directory, codified. This page is the human-readable index of what it does
autonomously and when it hands back to you.

## Codified runbooks (AUTO-01a–c)

The control policy lives in `services/autopilot/internal/plan`. Each recognized
condition maps to an ordered remediation runbook:

| Condition (signal → severity floor) | Runbook | Task |
|---|---|---|
| `data_gap` ≥ critical | quarantine the data subject | AUTO-01b |
| `reconcile_divergence` ≥ warning | quarantine the divergent subject | AUTO-01b |
| `drift` ≥ critical | roll the model back (MLOPS-01e in reverse) | AUTO-01b |
| `staleness` ≥ critical | scale out `market-data` ingest | AUTO-01c |
| `circuit_open` ≥ warning | scale out `inference` | AUTO-01c |
| `slo_burn` ≥ critical | scale out `risk-engine` (+ failover if enabled) | AUTO-01c |

Signals are the DATA-07 quality events (gap/staleness/drift), DATA-05
reconciliation divergence, model drift, OBS-01 SLO fast-burn, and the PRED-07
inference circuit-breaker — normalized in `internal/signal`.

## When autopilot escalates to you (AUTO-01d)

Escalation (`AutopilotEscalation` page) fires when the loop will NOT act
autonomously, so a human decides:

- **Unrecognized / sub-threshold condition** — a signal that matches no runbook,
  e.g. a WARNING data gap below the auto-quarantine floor. Autopilot deliberately
  does not act on soft signals.
- **A remediation step failed** — the quarantine/rollback/scale call errored.
  Autopilot stops the runbook and pages rather than retrying blindly.
- **Regional failover** — the hardest-to-reverse action. With
  `AUTOPILOT_AUTO_FAILOVER` off (the default), a sustained critical SLO burn only
  auto-scales and escalates the failover decision to you; turn it on only for
  environments where autonomous DR-01d failover is acceptable.

## Confirm

```promql
# What has autopilot done recently, remediated vs escalated?
sum by (condition, outcome) (rate(kanz_autopilot_outcomes_total[15m]))
```

A rising `outcome="escalated"` with no matching `remediated` means a condition is
recurring that autopilot can't fix — treat it as the real incident and use that
condition's own runbook in this directory.

## Mitigate / Recover

Autopilot's actions are the mitigations. To clear a quarantine or undo a model
rollback once the root cause is fixed, use the data-quality / model-promotion
runbooks; autopilot does not auto-un-quarantine (re-admitting bad data is a human
decision). To pause autopilot, scale its Deployment to zero — the platform keeps
running, only the autonomous remediation stops.

## Root-cause pointers

The escalation log line (`autopilot human escalation`) carries the signal kind,
subject, severity, and reason. Cross-reference the triggering `event_id` in the
audit log (AUDIT-01) and its lineage (LIN-01) to find what produced the bad
signal.
