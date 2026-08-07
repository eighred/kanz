# Dashboards — one that renders, three that were deleted, and the contract they defined

## `slo-burn-rate.json` — the SLO board (#85)

The one dashboard here. It renders the burn rates
[`../slo/slo.recording.rules.yaml`](../slo/slo.recording.rules.yaml) computes,
and it exists because it satisfies the ordering the rest of this file was
written to insist on: **the series it needs are emitted, a Prometheus scrapes
them (#61), and the rules evaluate.** That is the difference between it and the
three below — not that it is newer or better reviewed, but that every panel has
a producer underneath it.

Two properties are load-bearing and worth preserving through any edit:

- **No panel names a service or an SLO.** Every query is label-generic over
  whatever the catalog contains, so an SLO added to `slo.yaml` and compiled into
  the rules appears on the board with no JSON edit. `TestSLODashboardsDoNotPinAServiceOrSLO`
  enforces it. A hard-coded panel list is how a board ends up covering four SLOs
  out of five and still looking green.
- **An SLO with no data is rendered, not dropped.** The stat and table panels
  carry an `or on(service, slo) (slo:error_budget:ratio * 0 - 1)` tail: the
  budget series is a `vector()` literal that exists for every SLO in the
  catalog, so the tail substitutes a sentinel for any SLO whose SLI series is
  absent and the panel shows `NOT MEASURED`. Without it the join silently drops
  that SLO — verified live, where the plain-join time-series panels returned 4
  series against a catalog of 5. A missing SLO and a healthy SLO must not look
  the same, and on a plain join they do.

`NOT MEASURED` (no series) and `IDLE` (series exists, zero denominator, so the
ratio is NaN) are deliberately distinct states, and neither is green. A silent
service burns no budget — correct for alerting, and not the same as success.

## Data-observability dashboards — REMOVED, and the metric contract they defined

`freshness.json`, `completeness.json` and `drift.json` were deleted (issue
#123). The metric contract below is kept, because it is the specification anyone
re-instrumenting this layer needs and it exists nowhere else.

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
today — verified by matching the way `TestEveryObservabilityMetricExistsInSource`
matches, against quoted string literals rather than any mention.

That is why they were deleted rather than kept as a target: a dashboard is not a
specification. It is a rendering of a specification, and a rendering that has
never once rendered is a liability — a human opens it during an incident and
reads empty panels as *quiet*, not as *this was never connected*. The
specification itself is worth keeping, so it is kept here, as text, where it
cannot be mistaken for a working view.

At the time this was written there was also nothing to render into: no
Prometheus existed in the estate at all. **That half has since changed** — #61
landed and was proven on the rig, which is what made `slo-burn-rate.json` above
possible. It changes nothing for the nine metrics below: a scraper with nothing
to scrape is still nothing to scrape, and re-adding these dashboards still
requires the exporter first.

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
2. Deploy a Prometheus that scrapes it (#61 — **done**, so this step is now
   "add the scrape annotation", not "build a monitoring plane").
3. Then write dashboards and alert rules against series that exist, and the SLO
   in [`../slo/`](../slo/README.md) against the staleness gauge that exists.

`slo-burn-rate.json` is what that order looks like when it is followed, and it
is worth comparing against: its steps 1 and 2 were already true before a line of
JSON was written.

Two guards enforce step 3 against steps 1 and 2, and they cover different
vocabularies:

- `TestEveryObservabilityMetricExistsInSource` — a dashboard or rule naming a
  `kanz_*` metric the Go source does not emit fails the build. This is the guard
  that would have caught the three deletions above.
- `TestEverySLORecordingRuleReferenceIsProduced` — a dashboard or alert naming a
  derived `slo:*` series the recording rules do not produce fails the build. The
  first guard says nothing about derived series, and every SLO panel and every
  burn-rate alert is written entirely in derived series.
