# Load / soak harness (SRE-01e)

Establishes the platform's **baseline capacity** and enforces the **LATENCY-01
p99 budget** (≤ 500ms, the same number the CICD-01e canary and the SRE-01a
gateway SLO use). Drives the api-gateway read surface (API-01c): exposure,
measures, scenario.

## Tooling: k6

[k6](https://k6.io/) — a single Go binary, scripts in JS, **native p99
thresholds** that exit non-zero on breach, and open-model executors
(arrival-rate) that measure *request rate the system sustains* rather than
fixed-VU throughput. The threshold-as-exit-code is exactly what LATENCY-01a
needs to gate regressions in CI; no infra to stand up (no dependency bloat).

| File | Purpose |
|---|---|
| `config.js` | endpoints, request mix, and the single source of the p99/p95/error budgets |
| `baseline.js` | ramp arrival rate until budget breaks → capacity number |
| `soak.js` | steady sub-capacity load for hours → leaks / p99 drift |

## Run

```sh
# Baseline capacity — find the RPS knee within the 500ms p99 budget.
k6 run -e BASE_URL=https://gw.kanz.example baseline.js

# Soak — hold ~60% of measured capacity and watch for drift.
k6 run -e BASE_URL=https://gw.kanz.example -e RATE=300 -e DURATION=2h soak.js
```

Env: `BASE_URL` (gateway), `PORTFOLIO` (default `PF1`), `TOKEN` (bearer, if the
AUTH-01 edge requires it — pass at invocation, never commit), `RATE`/`DURATION`
(soak).

## Reading the result

- **Baseline capacity** = the arrival rate at the last stage before
  `http_req_duration p(99)` or `http_req_failed` goes red (`baseline.js` aborts
  there). That RPS is the headline capacity number; size soak `RATE` and SLO
  expectations off it.
- **Load-shed vs. error**: 429/503 are counted in `requests_shed`, distinct from
  `http_req_failed`. Shedding under overload is the gateway's quota/admission
  (MT-01e) working — it caps the *measured* capacity, it isn't a failure.
- **Soak health** = `soak_req_duration p(99)` flat across the window. A rising
  trend at constant load is a leak — the whole point of the soak.

## CI gate (LATENCY-01a)

The budgets live in `config.js` (`P99_BUDGET_MS` etc.) and are enforced as k6
thresholds, so LATENCY-01a runs `baseline.js` against an ephemeral stack and
fails the pipeline on a budget breach — no separate assertion to keep in sync.
The matching in-process micro-benchmark guard is LATENCY-01d (`testing.B`).
