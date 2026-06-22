# Service SLOs & error-budget policy (SRE-01a)

`slo.yaml` is the platform's SLI/SLO **contract** — the per-service indicators
and objectives, in a shape SRE-01b compiles into Prometheus recording rules and
multi-window burn-rate alerts. This file defines *what we promise*; SRE-01b
defines *how we measure and alert on it*. Definition lands before wiring, the
same discipline as the [DATA-08 metric contract](../dashboards/README.md) and
the [CICD-01e canary template](../../deploy/analysis-template.yaml).

## The SLOs

| Service | SLI | Objective | Source series |
|---|---|---|---|
| api-gateway | availability (non-5xx) | 99.9% | `kanz_gateway_requests_total` (MT-01e) |
| api-gateway | query latency < 500ms | 99% | `kanz_gateway_request_duration_seconds` |
| risk-engine | recompute success | 99.5% | `kanz_risk_recompute_total{status}` (OBS-01c) |
| risk-engine | recompute latency < 500ms | 99% | `kanz_risk_recompute_duration_seconds` |
| event-bus | delivery success | 99.9% | `kanz_bus_consume_total{result}` (OBS-01c) |
| market-data | freshness ≤ 10s | 99% | `kanz_data_staleness_lag_seconds` (DATA-02) |

Every SLI binds to a series that already exists, so no new instrumentation is
required — only the recording/alert rules SRE-01b generates from this file.

### Why these objectives

- **Steady-state ≠ deploy gate.** The risk-engine SLO (99.5% / 99% under 500ms)
  is deliberately tighter than the CICD-01e canary gate (99% / 500ms p99): a
  canary may run hotter for a few minutes than the platform promises over 28
  days. Same metric, two purposes.
- **Latency thresholds reuse the ORCH-01f 500ms budget** — the one the e2e
  harness and SRE-01e load/soak test also assert — rather than inventing a new
  number.
- **Admission 503 counts against gateway availability** (it is not carved out).
  A deliberate load-shed is still a request the client did not get served;
  letting it burn budget creates the pressure to add capacity (the INFRA-01c
  autoscaling premise) instead of hiding shed load behind a "transient" code.
- **A DLQ-routed message counts as a bus *error*.** The DLQ is the safety net
  the SRE-01c chaos suite verifies, not a success path.

## Error-budget policy

Budget over a rolling **28-day** window: `error_budget = 1 − objective`
(e.g. 99.9% ⇒ 0.1% = ~40 min/28d of allowed unavailability).

| Budget remaining | Posture |
|---|---|
| > 50% | Normal velocity. Ship features; reliability work is routine. |
| 10–50% | Caution. New rollouts require a clean canary; reliability bugs jump the backlog. |
| < 10% | **Change freeze** on the affected service — only reliability/rollback changes ship until the budget recovers. |
| Exhausted (≤ 0) | Freeze + **mandatory postmortem** (feeds SRE-01d runbooks-as-code). The error-budget breach is the trigger, not an individual page. |

Burn-rate alerts (SRE-01b) are the *leading* signal: a fast-burn page means the
budget will be gone soon at the current rate, well before the table above trips.
The freeze tiers are the *lagging* policy lever once it actually is gone.

### Burn-rate windows

SRE-01b emits these multi-window/multi-burn-rate pairs per SLO (declared in
`slo.yaml` `defaults.burn_rates`); the short window guards against alerting on a
stale long window:

| Severity | Long / short window | Burn factor | Budget spent |
|---|---|---|---|
| page | 1h / 5m | 14.4× | ~2% in 1h |
| page | 6h / 30m | 6× | ~5% in 6h |
| ticket | 1d / 2h | 3× | — |
| ticket | 3d / 6h | 1× | — |

`severity` drives Alertmanager routing exactly as the DATA-09 rules do
(`critical`/page ⇒ on-call, `warning`/ticket ⇒ tracker).

## Rules (SRE-01b)

`slo.yaml` is compiled into Prometheus rules:

| File | Contents |
|---|---|
| `slo.recording.rules.yaml` | per-SLO error budget (`slo:error_budget:ratio`) + the SLI error ratio (`slo:sli_error:ratio_rate<window>`) over the seven burn-rate windows |
| `slo.alerts.rules.yaml` | four multi-window burn-rate alerts (`SLOFastBurn`/`SLOSlowBurn`/`SLOBudgetBurnTicket`/`SLOBudgetBurnChronic`), generic over every SLO via `on(service, slo)` |
| `slo_test.yaml` | promtool unit tests pinning the burn-rate wiring |

The alerts are **generic**: one rule per burn pair fires for *any* `{service,slo}`
whose recorded error ratio exceeds `factor × slo:error_budget:ratio` on both the
long and short window. Adding a service (SRE-01a) needs only its error-ratio
recording rules + a budget series — no new alert rules. This extends the DATA-09
data-quality alerts onto the same Alertmanager; `layer="slo"` scopes routing.

Wire into Prometheus and validate exactly as the DATA-09 rules:

```sh
promtool check rules slo.recording.rules.yaml slo.alerts.rules.yaml
promtool test rules slo_test.yaml
```

```yaml
# prometheus.yml
rule_files:
  - /etc/prometheus/rules/slo.recording.rules.yaml
  - /etc/prometheus/rules/slo.alerts.rules.yaml
```

> The recording rules carry each objective as a literal `vector(...)` budget
> series. Change an objective in `slo.yaml` **and** its `slo:error_budget:ratio`
> rule together — `slo.yaml` is the source of truth, the rule is its compile.

## Scope notes

- **Per-subject / per-tenant budgets** (a sub-second tick feed vs. an EOD batch;
  a noisy tenant) are deployment overrides layered on top of these defaults, not
  changes here — the same per-`subject` override stance as the DATA-09 alerts.
- **Adding a service**: append a `services[]` entry with SLIs bound to its
  RED series; SRE-01b picks it up with no rule hand-editing.
