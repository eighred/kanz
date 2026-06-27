# performance service (PERF-01c)

The performance measurement & attribution reporting plane — "how did we do and
why" (ROI #28). A **stateless analytics HTTP service** over
`internal/performance`: it computes time-/money-weighted returns,
benchmark-relative active return, Brinson attribution, and ex-post risk from
request-supplied inputs, so it needs no broker or database to serve and boots
ready immediately.

## Boundary

`internal/performance` lives **outside** `internal/risk`, so per the RISK-02 arch
boundary it carries its **own** seams (a `Classifier` for sector bucketing, value/
flow providers) rather than importing the risk impl packages. The point-in-time
price history it reads is the shared `internal/marketdata/store` (not
risk-internal), so no boundary is crossed — the architecture test confirms no new
edge.

## Endpoints

| Method / path        | Body                                                            | Returns |
|----------------------|----------------------------------------------------------------|---------|
| `GET /healthz`       | —                                                              | liveness |
| `GET /readyz`        | —                                                              | readiness |
| `GET /metrics`       | —                                                              | Prometheus |
| `POST /v1/returns`   | `sub_periods` (TWR) and/or `begin_value`/`end_value`/`flows`   | TWR + Modified-Dietz + IRR money-weighted returns |
| `POST /v1/attribution` | `periods` (Carino-linked) or `sectors` (single period)       | Brinson allocation/selection/interaction by sector |
| `POST /v1/risk`      | `portfolio`, `benchmark`, `risk_free_per_period`, `periods_per_year` | tracking error, information ratio, Sharpe, beta |

## Config

| Env                          | Default | Meaning |
|------------------------------|---------|---------|
| `PERFORMANCE_LISTEN`         | `:8080` | HTTP listen address |
| `PERFORMANCE_LOG_LEVEL`      | `info`  | slog level |
| `PERFORMANCE_OTLP_ENDPOINT`  | —       | OTel collector for span export |

## Point-in-time

Returns/attribution carry an `as_of` knowledge horizon: the bitemporal store read
is bounded at `as_of`, so a late vendor restatement never leaks backward into a
historical performance window (the MODEL-01i no-future-leakage contract — see the
`internal/performance` valuation tests). Auto-valuing a portfolio from its live
position history (vs request-supplied valuations) is a carried-forward wiring,
the same reference/position-store integration the risk engine awaits.
