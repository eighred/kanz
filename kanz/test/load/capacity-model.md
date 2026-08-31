# Capacity model & autoscaling policy (PARITY-05d)

Derives, from the load harness, **how much one replica of each hot-path service
sustains within the LATENCY-01 budget** and **the KEDA/HPA policy that turns
that number into a replica count**. It closes the loop the SRE-01e harness
opened: `baseline.js` measures the knee, this file turns the knee into a scaling
threshold and a headroom plan.

The one budget everything is sized against (from `config.js`): **read-path p99 ≤
500 ms, error rate ≤ 0.1 %**. Capacity is defined as *the load at which p99 is
still under budget* — not the maximum the box can push.

## Harness → what each tool measures

| Tool | Path exercised | Capacity output |
|---|---|---|
| `seed` (`SEED_PORTFOLIOS=N`) | seeds an N-portfolio book so load is not one hot key | book size the run is valid for |
| `baseline.js` (`PORTFOLIOS=N`) | **read** hot path (gateway → risk-engine query), spread across the book via `pickPortfolio()` | sustained **RPS/replica** at the p99 knee |
| `ingest` (`RATE`, `DURATION`, `PORTFOLIOS`) | **state-write** hot path (NATS → risk-engine ingest → sharded recompute, PARITY-05a) | sustained **events/sec/replica** before bus-pending climbs |
| `orderflow` (`STAGES`, `STAGE_DURATION`) | **ORDER ADMISSION** (gateway `POST /v1/orders` → broker → OMS claim → pre-trade gate → Postgres admission → outbox) | sustained **admitted orders/sec/replica**, and which control degraded first |
| `soak.js` | steady sub-capacity read load for hours | p99 drift / leaks |

Production-shaped run (single command per side):

```sh
# Book: 2,000 portfolios.
SEED_PORTFOLIOS=2000 go run ./test/load/seed
# Read capacity across the book.
k6 run -e BASE_URL=$GW -e PORTFOLIO=PF1 -e PORTFOLIOS=2000 baseline.js
# Write capacity: 5k position ticks/sec for 2 min across the book.
RATE=5000 DURATION=2m PORTFOLIOS=2000 go run ./test/load/ingest
```

## The capacity model

Let, per service, `C1` = sustained load per replica at the p99 knee (measured),
`L` = offered load, `H` = headroom factor (spare capacity for spikes + one
in-flight rolling-deploy replica), `R` = replicas. Then:

```
R = ceil( L / C1 * H ),   clamped to [minReplicaCount, maxReplicaCount]
```

`H = 1.5` is the default: 33 % steady-state headroom so a scale-up has room to
react before the budget breaks, plus cover for one replica out during a rollout.

The KEDA trigger `threshold` is set to the **per-replica signal value that
corresponds to `C1`**, so the HPA math (`desired = ceil(Σsignal / threshold)`)
reproduces the formula above without KEDA needing to know `C1` directly.

### risk-engine — read (query) and write (ingest)

Two loads share the replicas. The binding constraint is whichever needs more.

- **Read**: the query server is CPU-bound on compute (`ComputeMeasures`). `C1_read`
  ≈ the `baseline.js` knee RPS. The gateway in front scales on CPU (70 %, its
  ScaledObject); risk-engine query load tracks the **bus-pending** signal only
  indirectly, so read scaling is governed by the gateway + the query server's own
  CPU HPA.
- **Write**: ingest → recompute is the sized path. The scale signal is
  `kanz_bus_pending_messages{group="risk-engine"}` (INFRA-01c). Threshold **500
  pending/replica** = `C1_write` expressed as standing backlog: one replica works
  ~500 queued state events down inside the recompute debounce + p99 window. Lag
  past that ⇒ add workers.

**Sharding interaction (PARITY-05a).** With consistent-hash sharding on, each
replica owns ~`1/R` of portfolios and subscribes as its own broadcast group, so
the recompute (the expensive part) is partitioned — `C1_write` scales ~linearly
in `R` until the per-replica *decode* of the broadcast stream (cheap) or Postgres
write throughput becomes the ceiling. The pending threshold stays per-replica;
`maxReplicaCount 12` is the point past which broadcast-decode duplication starts
to outweigh the recompute win for the current book — re-measure before raising it.

`risk-engine-scaledobject.yaml`: `min 3` (INFRA-01d quorum / multi-AZ), `max 12`,
scaleUp window 30 s (react to a building backlog), scaleDown 300 s (don't thrash).

### market-data — write (tick fold)

Scale signal `kanz_bus_consumer_lag{group="market-data"}`
(`market-data-scaledobject.yaml`, added here). `C1` is set by the per-instrument
partitioning + batching (PARITY-05b): one replica's lanes drain ~**1000 messages**
of standing lag within the p99 budget, so the threshold is 1000 lag/replica.
Because lanes parallelize per instrument, market-data scales further than risk
(`max 16`). The batch amortization (`PublishBatch`) is what keeps `C1` high under
production tick rates — without it, per-event publish overhead caps `C1` an order
of magnitude lower.

### OMS — order admission (#865)

Until #865 this section did not exist, and that absence was the point of the
issue: the platform could state its read-path p99 and could not state how many
orders per second it admits before a control degrades.

`orderflow` measures it. The scale signal is the same family the risk-engine
already uses — `kanz_bus_pending_messages{group="oms",subject="order.order.submit"}`
— which is the standing backlog of order COMMANDS. **It is not yet a KEDA
threshold, deliberately.** The OMS is not horizontally scaled on it today, and
sizing a ScaledObject off a single-node figure would put a number in a manifest
that the estate would then treat as derived. What follows is the first
measurement, not a policy.

#### The local figure, and what it is not

Measured 2026-08-31 on ONE developer machine — Windows 11, i5-12500H (12 cores /
16 threads), 16 GB, Postgres 16 and NATS 2.x in Docker Desktop, one OMS replica,
one api-gateway, all on the same box as the load generator. **These are local
figures and not a capacity guarantee.**

Idle box (nothing else running):

| offered orders/s | admitted | p50 | p95 | p99 | peak outstanding | verdict |
|---|---|---|---|---|---|---|
| 20 | 399/399 | 8ms | 13ms | 21ms | 1 | ok |
| 40 | 799/799 | 27ms | 241ms | 278ms | 10 | ok |
| 60 | 1199/1199 | 5.17s | 10.42s | 10.83s | 430 | **saturated** |

Same box, same code, while `go test ./services/...` was running beside it:

| offered orders/s | admitted | p50 | p99 | verdict |
|---|---|---|---|---|
| 5 | 99/99 | 8ms | 22ms | ok |
| 10 | 199/199 | 8ms | 110ms | ok |
| 14 | 279/279 | 2.28s | 6.49s | **saturated** |
| 25 | 373/373 | 19.9s | 70.2s | **saturated** |

Read it as: **the knee is between 40 and 60 admitted orders/sec on one idle
replica, and it collapses to single digits when the machine is contended.** That
spread — a factor of four to eight, from nothing but a busy neighbour — is the
concrete reason this is not a per-PR CI gate: on a shared runner the measurement
would be of the runner.

Four things in those tables are worth more than the number:

- **p50 barely moves while p99 explodes.** At 60/s the median order is still
  admitted in 5s while the 99th takes 10.8s; at 40/s the median is 27ms and the
  95th is already 241ms. That is the signature of a single dispatch goroutine per
  subject (`pkg/bus`: one bus subject is dispatched by ONE goroutine) — one slow
  order stalls everything queued behind it, and a harness reporting means would
  have called every stage healthy.
- **HTTP stayed green throughout.** Every stage above answered 202 to essentially
  every submission, including the one where admission took seventy seconds. The
  degradation is visible only from the FACT side, which is why this harness reads
  the bus at all.
- **The bus gauge corroborates but cannot lead.** At 60/s the harness's own
  outstanding count peaked at 430 while `kanz_bus_pending_messages` read 101 —
  the gauge is refreshed every 15s per subscription, so on a 20s plateau it sees
  one or two polls. It agrees on direction and understates the depth.
- **Exactly-once held at every rate measured**, including the stage whose p99
  (70s) exceeded the consumer's own 60s `AckWait`: across the runs above, every
  accepted submission produced exactly one terminal FACT and none produced two
  (2397/2397 on the idle ramp, 522/522 on the contended one). That is the
  idempotent-handler floor doing its job, and it is the first time it has been
  observed under sustained concurrency rather than argued from a lock comment.

#### What it does NOT measure

- **The exchange hop.** Orders were filled by an in-process `SimVenue`; a real
  venue adapter adds a network round-trip per order *inside the same delivery
  budget*, so the live figure is lower and this number must never be quoted as an
  execution capacity.
- **Multi-replica behaviour.** One OMS replica, one gateway.
- **`-race`.** It needs cgo and does not run on this box; CI is the detector.

Re-run it against a real multi-node deployment before any of this becomes a
threshold. The command and the stack recipe are in `README.md`.

### Phase-7 read services (wealth/datamaster/copilot/alternatives)

Stateless HTTP, no bus — scale on **CPU 70 %** (`phase7-scaling.yaml`, SVCWIRE-01d).
`C1` = RPS at 70 % CPU; `H` is baked into the 70 % target (30 % headroom). `min 2`
(PDB floor), `max 8`.

## Policy summary

| Service | Signal | Threshold/replica (`C1`) | min | max | Manifest |
|---|---|---|---|---|---|
| risk-engine (write) | `kanz_bus_pending_messages` | 500 pending | 3 | 12 | `risk-engine-scaledobject.yaml` |
| market-data | `kanz_bus_consumer_lag` | 1000 lag | 2 | 16 | `market-data-scaledobject.yaml` |
| wealth/datamaster/copilot/alternatives | CPU | 70 % util | 2 | 8 | `phase7-scaling.yaml` |
| api-gateway | CPU | 70 % util | — | — | (SRE-01a) |
| oms (admission) | `kanz_bus_pending_messages{subject="order.order.submit"}` | **not set** — measured, not yet policy (#865) | — | — | none |

## Re-derivation cadence

The thresholds above are the current best estimates tied to the harness
structure; the **authoritative** numbers come from running `baseline.js` /
`ingest` against a real multi-node deployment (the single-node compose stack
cannot produce a valid knee — see README). Re-run and re-fit `C1` when: the
compute cost per portfolio changes materially (new measures), the book grows past
the run's `PORTFOLIOS`, or a scale ceiling (`maxReplicaCount`) is hit in prod. The
`capacity-baseline` GitHub workflow (`workflow_dispatch`) runs the full ramp for
exactly this.
