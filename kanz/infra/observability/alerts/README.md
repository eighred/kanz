# Data-quality alerting

Prometheus alerting rules for the data-integrity layer (DATA-09), over the same
`kanz_data_*` metric contract the [dashboards](../dashboards/README.md) render.

| File | Groups | Covers |
|---|---|---|
| `data-quality.rules.yaml` | freshness, completeness, drift, quality-events | staleness (DATA-02), gaps (DATA-01) + reconciliation (DATA-05), drift (DATA-04), DATA-07 emission stream + exporter liveness |

## Thresholds

Thresholds mirror the detectors' own defaults, so a firing alert means "the
detector would flag this":

| Alert | Expr threshold | Source default | Severity |
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

Per-stream tuning is a deployment concern — a sub-second tick feed and an
end-of-day batch want different staleness budgets; override per `subject` with
additional rules rather than loosening these defaults globally.

The drift rules compare `kanz_data_drift_score` against the exported
`kanz_data_drift_threshold` (`> on (feature, metric)`), so the alarm point
tracks each detector's own configured threshold automatically — no threshold is
duplicated between the detector and the rule.

## Wire into Prometheus

```yaml
# prometheus.yml
rule_files:
  - /etc/prometheus/rules/data-quality.rules.yaml
```

Mount this file at that path (ConfigMap in-cluster, or the Prometheus Operator
`PrometheusRule` CRD). Validate before shipping:

```sh
promtool check rules data-quality.rules.yaml
```

## Alertmanager routing

`severity` drives routing; `layer="data-integrity"` scopes it:

```yaml
# alertmanager.yml (excerpt)
route:
  routes:
    - matchers: [ 'layer="data-integrity"', 'severity="critical"' ]
      receiver: data-oncall-page
    - matchers: [ 'layer="data-integrity"', 'severity="warning"' ]
      receiver: data-tickets
```
