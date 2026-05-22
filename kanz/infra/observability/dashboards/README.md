# Data-observability dashboards

Grafana dashboards for the data-integrity layer (`kanz/internal/integrity`,
DATA-01..07). Three views, one per integrity dimension:

| File | Dashboard | Covers |
|---|---|---|
| `freshness.json` | Data Freshness | staleness lag + stale-frontier age (DATA-02) |
| `completeness.json` | Data Completeness | sequence gaps (DATA-01) + NATS↔Kafka reconciliation (DATA-05) |
| `drift.json` | Input Drift | distribution-drift scores vs threshold (DATA-04) |

All three query a Prometheus datasource (selected via the `datasource`
dashboard variable) and a free-text `subject` regex variable for filtering to
one stream.

## Metric contract

The dashboards depend on the metric names below. **This is the contract the
integrity layer's instrumentation must satisfy** — the detectors (DATA-01..05)
are pure in-memory classifiers today; exposing these gauges/counters (a thin
Prometheus exporter wrapping the detectors, or derived by a consumer of the
DATA-07 `DataQualityEvent` stream) is wired separately. Defining the names here
first lets the dashboards and the DATA-09 alert rules target a stable surface,
the same detection-lands-before-wiring discipline the rest of the layer follows.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `kanz_data_staleness_lag_seconds` | gauge | `subject`, `partition_key` | `ingestion_time - event_time` (DATA-02 `Lag`) |
| `kanz_data_last_event_age_seconds` | gauge | `subject`, `partition_key` | `now - last_event_time` — stale-frontier age |
| `kanz_data_gap_missing_total` | counter | `subject`, `partition_key`, `source` | missing `producer_sequence` values detected (DATA-01 `MissingCount`) |
| `kanz_data_reconcile_pending` | gauge | `transport` | events awaiting cross-transport confirmation (DATA-05 `PendingCount`) |
| `kanz_data_reconcile_discrepancies_total` | counter | `seen_on`, `missing_on` | one-sided events that aged past `MatchDeadline` |
| `kanz_data_reconcile_match_latency_seconds` | histogram | — | wall-clock NATS↔Kafka match gap (DATA-05 `MatchLatency`) |
| `kanz_data_drift_score` | gauge | `feature`, `metric` | latest drift score (DATA-04 `Score`) |
| `kanz_data_drift_threshold` | gauge | `feature`, `metric` | configured alarm threshold (DATA-04 `Threshold`) |
| `kanz_data_quality_events_total` | counter | `kind`, `severity`, `subject` | `DataQualityEvent`s published (DATA-07), by kind/severity |

Label values reuse the detector vocabulary: `subject` = the upstream stream
(`market.equity.trade`), `kind` ∈ {`gap`,`staleness`,`drift`}, `severity` ∈
{`warning`,`critical`}, `transport`/`seen_on`/`missing_on` ∈ {`nats`,`kafka`}.

## Provision

The JSON models are Grafana provisioning artifacts. Mount this directory into
Grafana via a dashboard provider:

```yaml
# /etc/grafana/provisioning/dashboards/kanz-data.yaml
apiVersion: 1
providers:
  - name: kanz-data-integrity
    folder: Data Integrity
    type: file
    options:
      path: /var/lib/grafana/dashboards/kanz-data
```

…and ship the three JSON files to that `path` (ConfigMap mount in-cluster, or
the Grafana Helm chart's `dashboards` values). They are also importable by hand
via **Dashboards → Import**.
