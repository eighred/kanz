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

## Data quality is still observed — on the event path, not the metrics path

This is the part worth not losing. Deleting these rules did **not** leave the
platform blind to data quality. `observation.v1.DataQualityEvent` still flows on
the bus and still has live consumers:

- `services/audit` classifies and records it (`internal/audit/classify.go`,
  `record.go`'s `KindDataQuality`), and it surfaces in the `data-quality` audit
  report template.
- `services/autopilot` matches gap/staleness/drift signals and runs remediation
  runbooks against them (`internal/signal`, `internal/controller`).

So the FACT-grade signal survived; what died was its Prometheus export. That
distinction matters when re-instrumenting: the events are already there, and
what is missing is a component that observes them and exposes counters/gauges.

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

The guard that stops this recurring is tracked as **#63** — an arch test
asserting every metric named under `alerts/` and `slo/` exists in the Go source.
Until it lands, nothing prevents a rule outliving its producer again.
