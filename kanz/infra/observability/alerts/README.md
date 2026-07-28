# Data-quality alerting — REMOVED, and why

There are no alerting rules in this directory. `data-quality.rules.yaml` was
deleted, not disabled, and this file records what it was and what bringing it
back requires.

## What happened

The rules alerted on the `kanz_data_*` metric contract, which was exported by
`internal/integrity`. That package was deleted in DATA-M4, and nothing has
emitted the series since — verified by searching the Go source for each of the
eight metric names and for every fragment of them:

```
kanz_data_drift_score                       0 Go files
kanz_data_drift_threshold                   0
kanz_data_gap_missing_total                 0
kanz_data_quality_events_total              0
kanz_data_reconcile_discrepancies_total     0
kanz_data_reconcile_match_latency_seconds   0
kanz_data_reconcile_pending                 0
kanz_data_staleness_lag_seconds             0
```

Left in place, the rules were worse than absent. Nine of the ten alerts could
never fire, because a threshold comparison against a series with no producer
yields an empty vector — a dashboard reader saw a fully-wired data-integrity
alerting layer with zero firing alerts, which is indistinguishable from healthy.
The tenth, `DataQualityMetricsMissing`, was
`absent(kanz_data_quality_events_total)`: the one rule that *could* fire, and it
would have fired continuously and forever, training whoever received it to
ignore the layer.

The market-data freshness SLO in [`../slo/`](../slo/README.md) was compiled from
the same orphaned `kanz_data_staleness_lag_seconds` gauge and was removed in the
same change.

## Data quality is still RECORDED — but nothing acts on it

Deleting these rules did not leave the platform blind to data quality.
`observation.v1.DataQualityEvent` still flows on the bus. What consumes it needs
stating precisely, because the two consumers are not in the same state:

- **`services/audit` — deployed, and genuinely live.** It classifies and records
  the events (`internal/audit/classify.go`, `record.go`'s `KindDataQuality`) and
  surfaces them in the `data-quality` audit report template. It has a Dockerfile
  and `infra/deploy/audit-deploy.yaml`.

- **`services/autopilot` — NOT DEPLOYED, and currently not deployable.** Its
  controller matches gap/staleness/drift signals and runs remediation runbooks
  (`internal/signal`, `internal/controller`), and that code is real. But the
  service has no Dockerfile, therefore no image and no workload manifest, so it
  runs in no environment. See issue #124.

This correction matters, and it was got wrong here first. The original version of
this section cited both consumers as evidence that the signal survived, which
overstated it: the RECORDING half survived, the REMEDIATION half is code that
exists and executes nowhere. An operator reading the first version would conclude
that a data-quality gap still triggers a runbook. It does not.

So: the FACT-grade signal survived and is durably recorded; its Prometheus export
died with `internal/integrity`; and its automated response has never run. When
re-instrumenting, the events are already there — what is missing is both a
component that exposes them as metrics AND a deployable autopilot to act on them.

## Historical thresholds

Kept because the Go constants they mirrored (`DefaultStalenessConfig.WarnAfter`,
`DefaultStalenessConfig.CriticalAfter`, `DefaultPSIThreshold`,
`DefaultMatchDeadline`) were deleted along with `internal/integrity` — this table
and git history are now the only record of what the platform considered a
data-quality breach. **These are not live thresholds.**

| Alert | Expr threshold | Source default (now deleted) | Severity |
|---|---|---|---|
| `DataStalenessWarning` | lag > 10s for 2m | `DefaultStalenessConfig.WarnAfter` | warning |
| `DataStalenessCritical` | lag > 60s for 1m | `DefaultStalenessConfig.CriticalAfter` | critical |
| `DataSequenceGap` | any missing in 5m | — (any loss is critical) | critical |
| `DataReconcileDiscrepancy` | any in 10m | — (hot path ≠ log of record) | critical |
| `DataReconcileBacklogGrowing` | pending > 1000 for 5m | — (backlog) | warning |
| `DataReconcileMatchLatencyHigh` | p99 > 30s for 5m | `DefaultMatchDeadline` | warning |
| `DataInputDriftWarning` | score > threshold for 10m | `DefaultPSIThreshold` (0.25) | warning |
| `DataInputDriftCritical` | score > 2× threshold for 5m | — | critical |
| `DataQualityCriticalEvent` | any critical event in 5m | catch-all over DATA-07 | critical |
| `DataQualityMetricsMissing` | series absent for 10m | exporter liveness | warning |

Two design notes worth carrying forward:

- Per-stream tuning was a deployment concern — a sub-second tick feed and an
  end-of-day batch want different staleness budgets. The stance was to override
  per `subject` with additional rules rather than loosen the defaults globally.
- The drift rules compared `kanz_data_drift_score` against the *exported*
  `kanz_data_drift_threshold` with `> on (feature, metric)`, so the alarm point
  tracked each detector's own configured threshold and no threshold was
  duplicated between detector and rule. Re-instrumenting should keep that shape.

## Restoring this layer

Order matters, and it is the order that was violated to produce this state:

1. Instrument first. Something must export the `kanz_data_*` series — most
   naturally a consumer of the `DataQualityEvent` stream that already exists.
2. Then write rules against the series that exist, and the SLO in `../slo/`
   against the staleness gauge that exists.
3. Alertmanager routing for this layer was `layer="data-integrity"`, `severity`
   driving page vs. ticket — the same shape `../slo/slo.alerts.rules.yaml` uses
   with `layer="slo"`.

Two things sit outside that order and are worth knowing before starting:

- **An alert with no Prometheus is still nothing.** There is no Prometheus in
  this estate at all — `infra/observability/` ships rules, dashboards and SLOs,
  and nothing scrapes or evaluates any of them. That is issue **#61**, and it is
  part of why an entire orphaned alerting layer survived unnoticed: no evaluator
  ever tried to run these rules and found their series missing.
- **An alert nobody acts on is a ticket nobody opens.** Remediation lives in
  `services/autopilot`, which is not deployable today (issue **#124**).

The guard that stops the rule-outlives-its-producer failure recurring is **#63**
— an arch test asserting every metric named under `alerts/`, `slo/` and
`dashboards/` exists in the Go source. It is implemented in
`kanz/test/arch/observability_metrics_test.go`, and the three dashboards are
listed in its `metricSurfacesPendingRepair` allow-list until **#123** resolves
them; the guard's dead-entry check forces those entries out as each is fixed.
