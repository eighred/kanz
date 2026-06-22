---
title: SLO error-budget burn
alert: SLOFastBurn
severity: page
slo: "<from alert labels: service/slo>"
gameday: infra/chaos/gamedays/gameday-workflow.yaml
---

# SLO error-budget burn

## Symptom
A burn-rate alert is firing (`SLOFastBurn` page, or `SLOSlowBurn` /
`SLOBudgetBurnTicket` / `SLOBudgetBurnChronic`). The alert's `service`/`slo`
labels name which objective is burning; `burn_rate` names how fast.

## Confirm
```promql
# Current burn vs. the SLO's budget threshold (>1 means burning faster than the
# 14.4× fast threshold allows):
slo:sli_error:ratio_rate1h / (14.4 * slo:error_budget:ratio)
# Budget consumed so far (per SLO):
slo:sli_error:ratio_rate3d / slo:error_budget:ratio
```

## Impact
The named SLO's 28-day error budget is draining. Map the SLO to user impact:
`api-gateway/availability` → requests failing; `*/recompute-latency` → slow
risk; `market-data/freshness` → stale views; `event-bus/delivery-success` →
handler failures (check the DLQ).

## Mitigate
1. Identify the dominant error source for the burning SLI (5xx codes, error
   status, lagging subject) — the recording rule's underlying series.
2. If a recent rollout correlates, **abort the canary / roll back**
   (`kubectl argo rollouts abort <svc> -n kanz-services`, see infra/deploy).
3. If load-driven, shed or scale (gateway admission is already shedding as 503s
   if `kanz_gateway_admission_rejected_total` is rising → add capacity).

## Recover
Burn stops when `slo:sli_error:ratio_rate5m` drops back under
`14.4 * slo:error_budget:ratio`; the short window clears the alert within
minutes. Confirm with the "Confirm" queries trending down.

## Root-cause pointers
Per-SLO error series in `infra/observability/slo/slo.recording.rules.yaml`; the
RED dashboards; the error-budget **policy** (`infra/observability/slo/README.md`)
— a depleted budget triggers a change freeze + postmortem, not just this page.
