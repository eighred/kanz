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
| `seed/` | publishes `PortfolioSnapshot`s so the read path serves real data (`SEED_PORTFOLIOS=N` for a production book) |
| `ingest/` | drives the risk-engine's **state-write** hot path at a production tick rate (`RATE`/`DURATION`/`PORTFOLIOS`) |
| `orderflow/` | drives **ORDER ADMISSION** through the gateway's `POST /v1/orders` and reports the rate at which the first control degrades (#865) |
| `capacity-model.md` | derives per-replica capacity + the KEDA/HPA thresholds from the runs (PARITY-05d) |

Three paths, and they are not interchangeable. `config.js` measures **reads**.
`ingest/` measures the risk-engine folding **position state**. `orderflow/`
measures the controls that decide whether **capital moves**.

## Run

```sh
# Baseline capacity — find the RPS knee within the 500ms p99 budget.
k6 run -e BASE_URL=https://gw.kanz.example baseline.js

# Soak — hold ~60% of measured capacity and watch for drift.
k6 run -e BASE_URL=https://gw.kanz.example -e RATE=300 -e DURATION=2h soak.js
```

Env: `BASE_URL` (gateway), `PORTFOLIO` (default `PF1`), `TOKEN` (bearer — now
REQUIRED: the gateway refuses to start unauthenticated (SEC-M1), so an
unauthenticated run measures the latency of 401s), `RATE`/`DURATION` (soak).

Against the local stack in `docker-compose.yml`, mint one:

```bash
TOKEN=$(go run ./cmd/kanz-devtoken --secret load-secret --tenant PF1-tenant) \
  k6 run -e BASE_URL=http://localhost:8080 -e TOKEN=$TOKEN baseline.js
```

Against a real gateway, use a real SSO token — never commit either.

## Production-volume runs (PARITY-05d)

Drive a production-shaped book instead of the single `PF1` smoke portfolio:

```sh
# Seed an N-portfolio book, then spread reads + writes across it.
SEED_PORTFOLIOS=2000 go run ./test/load/seed
k6 run -e BASE_URL=$GW -e PORTFOLIO=PF1 -e PORTFOLIOS=2000 baseline.js      # read capacity
RATE=5000 DURATION=2m PORTFOLIOS=2000 go run ./test/load/ingest             # write capacity
```

`PORTFOLIOS>1` makes `pickPortfolio()` (config.js) hit `PF1-0000..PF1-NNNN`, so
no single aggregate is a hot key — this is what exercises the PARITY-05a shard
fan-out and realistic state/cache spread. The write generator streams
`PositionState` FACTs at `RATE`/sec; watch `kanz_bus_pending_messages` climb as
the backpressure/scale signal. `capacity-model.md` turns both knees into the
autoscaling thresholds.

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

## The write path (`orderflow/`, #865)

Everything above this section measures a **query**. `orderflow/` measures
**admission**: it submits real orders through the api-gateway's `POST /v1/orders`
at a ramping open-model rate and reports the rate at which the first control on
the capital path degrades, together with what degraded.

```sh
ORDERFLOW_TOKEN_FILE=./token \
STAGES=5,10,20,40 STAGE_DURATION=20s DRAIN_TIMEOUT=90s \
  go run ./test/load/orderflow
```

### What it saturates, and why that is a real control

The gateway's auth, trade-role and halt checks; the tenant-routed command publish;
the broker's real delivery contract; the OMS's per-order claim; the pre-trade
compliance / mandate / margin gate; the Postgres admission gate; the outbox relay
that commits the `ORDER_ACCEPTED` FACT in the same transaction as the order. All
of them, in that order, with nothing skipped.

**The exchange is the one thing simulated**, and the harness refuses to run unless
that is true. A load test may not place real orders, so the venue adapter's
exchange REST/WS hop is out of scope by construction — which makes this an
**admission** capacity, never an execution capacity.

### How it refuses

Fail-closed, on facts the system under test asserts about itself rather than on
anything the operator running it can set:

| Refusal | Why |
|---|---|
| the OMS's `/metrics` cannot be read | whether an order reaches an exchange is UNKNOWN, and a critical unknown fails closed |
| `kanz_oms_live_venue_adapters` / `kanz_oms_simulated_venues` are absent | same — an absent family reads as zero to every consumer, so "no live adapters" would be indistinguishable from "this build never answered" |
| `kanz_oms_live_venue_adapters > 0` | every submission would be an order at a real exchange |
| both gauges are zero | the router refuses a MIC it has no venue for: nothing would trade and nothing would be measured, so the run would report the throughput of the *refusal* path |
| the canary order is not admitted | one real order goes first, and the run stops unless *that order's own* `ORDER_ACCEPTED` FACT comes back — a 423, a 403, a refusal and a silence send an operator to four different places, so each is named |
| `kanz_compliance_ungoverned_orders_total` moved during the run | the portfolio is under no mandate, so the gate returns at its **first branch** — no rule engine, no book projection, no margin resolution. The run is reported INVALID and its numbers must not be quoted |

That last one is #859's lesson applied to the write side. `config.js` sent an
`as_of` the risk engine ignored and reported green throughput for a contract
nothing honoured; an ungoverned portfolio is the same defect one door over, and
the only difference is that a write-path version of it would be quoted as an
admission capacity.

### What it reports

- **admission latency**: submit → that order's terminal FACT, read off the bus.
  A gateway 202 means "the COMMAND was published", not "the order was admitted",
  so this is the number that degrades while HTTP stays perfectly green.
- **peak outstanding**: 202s issued minus terminal FACTs seen, sampled every
  100ms. Exact, and the primary saturation signal.
  `kanz_bus_pending_messages{group,subject="order.order.submit"}` is printed
  beside it as corroboration — it is refreshed by each subscription's own poller
  every 15s (`pkg/bus/backlog.go`), so it cannot resolve a short plateau and a
  reading of `0` there means "the polls that landed read 0".
- **exactly-once**: one submission, one announcement. Two FACTs for one order id
  is the shape a redelivery overtaking a still-running handler leaves behind.
- **control deltas**: the counters in `orderflow/scrape.go`, each with the
  sentence a non-zero delta means.

### The budget is derived, not invented

`pkg/bus/tuning.go` sizes the whole delivery contract by
`MaxAckPending × worst-case per-message handling < AckWait`, and the order-command
durable runs on the work class (`AckWait` 60s, `MaxAckPending` 32) — so handling
must stay under **1.875s**. That is the harness's stage threshold. Measured
submit→announce is *queue wait + handling* and the contract bounds handling alone,
so a p99 past the budget marks **saturation**, not a proven contract breach; the
peak-outstanding column beside it is what says the queue stopped draining.

### Why the write path is NOT a per-PR CI smoke

Deliberate, and the reason is not squeamishness:

- **The `docker-compose.yml` stack above cannot run it.** It is NATS +
  risk-engine + api-gateway. Admission needs an OMS and a Postgres order store —
  and it must be Postgres: with `OMS_DATABASE_URL` unset the admission gate is a
  process-local mutex and the outbox relay does not exist, so a run against the
  in-memory store would measure something production does not have.
- **A shared runner cannot produce a stable saturation point.** Measured: the same
  code on the same box saturated at **60 orders/sec idle** and at **14 orders/sec**
  while a `go test` run was going beside it — a factor of four from nothing but a
  busy neighbour (`capacity-model.md`). A gate with that spread flaps, a flapping
  gate gets muted, and a muted gate on the capital path is worse than an absent one.
- **The refusals are the CI-safe part, and they are unit-tested**
  (`preflight_test.go`) rather than exercised by standing a stack up.

So this is a **local / deliberate** run, like `baseline.js`'s full ramp. The
recipe below is the one that produced the numbers in `capacity-model.md`.

### Standing the stack up locally

`test/backing/up.sh` gives the same Postgres + NATS topology CI uses. Then:

```sh
# 1. An OMS schema in its own database (the shared kanzapp DB carries other
#    services' fixtures, and migration set 0003 collides with them).
docker exec kanz-ci-postgres psql -U kanz -d kanz -c "CREATE DATABASE kanzload OWNER kanzapp;"
KANZ_MIGRATE_DATABASE_URL='postgres://kanzapp:kanz@localhost:5432/kanzload?sslmode=disable' \
  go run ./cmd/kanz-migrate --dir ./services/oms/migrations --set oms

# 2. The OMS, in simulator posture (no OMS_VENUE_ENDPOINTS ⇒ SimVenue ⇒ the
#    posture gauges the harness refuses on).
OMS_NATS_URL=nats://localhost:4222 \
OMS_DATABASE_URL='postgres://kanzapp:kanz@localhost:5432/kanzload?sslmode=disable' \
OMS_TENANT=load-test OMS_LISTEN=:8090 OMS_PRICE_SUBJECTS='market.>' \
OMS_SIM_VENUE_MIC=XSIM OMS_DEFAULT_VENUE_MIC=XSIM \
  go run ./services/oms/cmd/oms &

# 3. The gateway. RISK_ENGINE_ADDR is required to start; the write path does not
#    use it, so any address will do when only orders are being measured.
API_GATEWAY_LISTEN=:8080 API_GATEWAY_NATS_URL=nats://localhost:4222 \
API_GATEWAY_RISK_ENGINE_ADDR=127.0.0.1:8081 \
API_GATEWAY_JWT_SECRET=load-secret API_GATEWAY_ALLOW_DEV_HS256=true \
API_GATEWAY_REQUIRED_ROLE=kanz-user API_GATEWAY_TRADE_ROLE=kanz-trader \
  go run ./services/api-gateway/cmd/api-gateway &

# 4. PUT THE PORTFOLIO UNDER MANDATE, or the run is INVALID and says so.
kanz-mandate propose --tenant load-test --file mandate.json --by operator:a \
  --reason "load rig" --out proposal.json
kanz-mandate approve --tenant load-test --file mandate.json \
  --proposal proposal.json --by operator:b --nats nats://localhost:4222

# 5. A token carrying BOTH roles and the portfolio entitlement. Without
#    --portfolio the OMS denies every order NOT_ENTITLED (#225) — the token
#    authenticates cleanly and the run measures refusals.
go run ./cmd/kanz-devtoken --secret load-secret --tenant load-test \
  --role kanz-user,kanz-trader --portfolio PF1 > ./token

ORDERFLOW_TOKEN_FILE=./token go run ./test/load/orderflow
rm -f ./token proposal.json     # the token moves capital; do not leave it behind
```

`ORDERFLOW_TOKEN_FILE` is preferred over `ORDERFLOW_TOKEN` for the reason every
`_FILE` variable in this estate exists: a bearer that can place an order does not
belong in a shell's environment or a process listing.

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
