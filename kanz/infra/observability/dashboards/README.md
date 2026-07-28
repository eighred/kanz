# Data-observability dashboards — REMOVED, and the metric contract they defined

There are no dashboards in this directory. `freshness.json`, `completeness.json`
and `drift.json` were deleted (issue #123). The metric contract below is kept,
because it is the specification anyone re-instrumenting this layer needs and it
exists nowhere else.

## What happened, and the part that is easy to get wrong

The obvious story is that `internal/integrity` was deleted in DATA-M4 and these
dashboards were left behind. That is true but incomplete, and the fuller version
matters more.

**These dashboards never rendered anything.** The previous version of this file
said so plainly: the detectors were *"pure in-memory classifiers today"*, and
exposing these gauges and counters — *"a thin Prometheus exporter wrapping the
detectors, or derived by a consumer of the DATA-07 `DataQualityEvent` stream"* —
was *"wired separately"*. Defining the names first was a deliberate choice, the
*"detection-lands-before-wiring discipline"* the layer followed.

The exporter never landed. Then the detectors it would have wrapped were deleted
too. So there was never a moment when a single one of these panels had data
behind it, and every one of the nine metrics below is absent from the Go source
today — verified by matching the way `TestEveryObservabilityMetricExistsInGo`
matches, against quoted string literals rather than any mention.

That is why they were deleted rather than kept as a target: a dashboard is not a
specification. It is a rendering of a specification, and a rendering that has
never once rendered is a liability — a human opens it during an incident and
reads empty panels as *quiet*, not as *this was never connected*. The
specification itself is worth keeping, so it is kept here, as text, where it
cannot be mistaken for a working view.

There is also nothing to render into: no Prometheus exists in this estate at all
(issue #61). Nothing scrapes, nothing evaluates.

## The metric contract (specification, NOT current state)

**None of these are emitted today.** This table is what an exporter must satisfy
for the DATA-01..07 layer to become observable. Names, types and labels are
preserved exactly as the layer defined them, so a future implementation lands on
the surface the alert rules and SLOs were written against rather than inventing a
second vocabulary.

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

Two design decisions in that table are load-bearing and should survive any
rewrite:

- **`kanz_data_drift_threshold` is exported alongside `kanz_data_drift_score`.**
  The deleted drift rules compared the two with `> on (feature, metric)`, so the
  alarm point tracked each detector's own configured threshold and no threshold
  was ever duplicated between detector and rule. Re-instrumenting without the
  threshold gauge forces that duplication back.
- **The three dimensions are separate metrics, not one metric with a `kind`
  label** — except `kanz_data_quality_events_total`, which is deliberately the
  cross-cutting count. Freshness, completeness and drift have different types
  (gauge vs counter vs histogram) and different label sets; collapsing them
  loses that.

## What still exists

Deleting these removed a rendering, not the signal.
`observation.v1.DataQualityEvent` still flows on the bus and `services/audit`
still classifies and records it (`KindDataQuality`, and the `data-quality` audit
report template). `services/autopilot` would remediate it, but is not deployable
— see issue #124.

So the recording path is live, the metrics path has no producer, and the
remediation path has no deployment.

## Rebuilding this

Order matters, and it is the order that was inverted to produce this state:

1. Build the exporter first — most naturally a consumer of the
   `DataQualityEvent` stream that already exists — emitting the contract above.
2. Deploy a Prometheus that scrapes it (#61).
3. Then write dashboards and alert rules against series that exist, and the SLO
   in [`../slo/`](../slo/README.md) against the staleness gauge that exists.

`TestEveryObservabilityMetricExistsInGo`
(`kanz/test/arch/observability_metrics_test.go`) enforces step 3 against steps 1
and 2: a dashboard or rule naming a metric the Go source does not emit fails the
build. It is what makes re-adding these safe.
