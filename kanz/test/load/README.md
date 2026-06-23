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
| `smoke.js` | short (~60s) low-rate run asserting the budgets → the LATENCY-01a CI gate |
| `baseline.js` | ramp arrival rate until budget breaks → capacity number |
| `soak.js` | steady sub-capacity load for hours → leaks / p99 drift |
| `docker-compose.yml` | the ephemeral stack the smoke/seed run against (NATS + risk-engine + api-gateway) |
| `seed/` | publishes one `PortfolioSnapshot` so the read path serves real data |

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

## Ephemeral stack

`docker-compose.yml` brings up the minimal slice the read path needs — NATS
(JetStream spine) + `risk-engine` (in-memory state, gRPC query server) +
`api-gateway` (auth/signing/quotas OFF, plaintext upstream). No Postgres/Kafka:
the engine re-derives state from the seeded snapshot on the live spine.

The service images COPY the **generated** kanz-schemas Go SDK
(`kanz-schemas/gen/go`, EVT-15a generated-not-committed), so `buf generate` must
run before `docker compose build` (the CI workflow does this; locally, generate
per the kanz onboarding flow):

```sh
docker compose -f kanz/test/load/docker-compose.yml up -d --build
(cd kanz && go run ./test/load/seed)          # publish PortfolioSnapshot for PF1
k6 run -e BASE_URL=http://localhost:8080 kanz/test/load/smoke.js
docker compose -f kanz/test/load/docker-compose.yml down -v
```

The seed exists because an unseeded portfolio returns `ErrPortfolioNotFound` →
404, which would trip the smoke's error budget. It reuses the production
`bus.Producer` so the envelope is stamped + validated exactly as a real
publisher would. Override `SEED_NATS_URL` / `SEED_PORTFOLIO` / `SEED_TENANT`.

## CI gate (LATENCY-01a)

The budgets live in `config.js` (`P99_BUDGET_MS` etc.) and are enforced as k6
thresholds, so the gate has no separate assertion to keep in sync.
`.github/workflows/latency.yml`:

- **Per PR** (`load-smoke`): on a change to the hot path (`internal/risk`,
  `pkg/bus`, the two services) or this harness, it spins up the ephemeral stack,
  seeds, and runs `smoke.js` — failing on a p99/error breach. A short, low-rate
  smoke (not the full ramp) keeps the per-PR gate fast and the p99 verdict
  stable on a shared runner.
- **Manual** (`capacity-baseline`, `workflow_dispatch`): runs the full
  `baseline.js` ramp against a real deployed env (`base_url` input). A dispatch
  with no `base_url` just runs the smoke against a fresh stack — the 4000-rps
  ramp is only meaningful against a real multi-node deployment, not the
  single-node compose stack.

The matching in-process micro-benchmark guard is LATENCY-01d (`testing.B`).
