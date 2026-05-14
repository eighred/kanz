# Event Class Handling Rules

Authoritative spec for the per-class consumer obligations of the `EventClass`
enum (`proto/envelope/v1/event_class.proto`). Every `Envelope` carries an
`event_class`; this document defines what a consumer is **permitted and
required** to do for each value.

Enforcement: the shared bus client library (EVT-17/18/19) implements the
publish-side rules; consumer-side rules are a contract every consumer must
honor.

## Summary

| Class | Delivery | Ordering | Dedup key | Mutable | Retention tier |
|---|---|---|---|---|---|
| FACT | at-least-once | per-partition | `event_id` | no | durable log (Kafka) |
| COMMAND | at-least-once | per-partition (per target) | `idempotency_key` (caller-set) | no | durable log (Kafka) |
| STATE_SNAPSHOT | at-least-once | latest-wins per aggregate | `event_id` | no | compacted |
| OBSERVATION | at-least-once, lossy-tolerant* | none required | `event_id` | no | short-retention (NATS) |

\* except data-quality observations — see §4.

## 1. FACT

A FACT records something that happened. It is immutable and past-tense:
market data, executions, applied state changes.

- **Delivery:** at-least-once. Consumers MUST be idempotent.
- **Dedup:** `idempotency_key` equals `event_id`. Consumers dedup against a
  bounded window sized to the broker's redelivery window.
- **Ordering:** per `partition_key`. Events sharing a key are totally ordered;
  across keys there is no guarantee. For domain/state FACTs, per-aggregate
  ordering (partition by aggregate id) is a **correctness requirement**, not an
  optimization — out-of-order application corrupts state.
- **Immutability:** a FACT is never mutated or retracted. A correction is a
  **new** FACT carrying `QUALITY_FLAG_REVISED` and a `causation_id` pointing at
  the event it revises.
- **Consumer obligations:** apply idempotently; respect `partition_key`
  ordering; treat `event_time` as authoritative for any
  model/backtest/windowing logic.

## 2. COMMAND

A COMMAND is a request that **may be rejected**. It is an intent, not a fact.

- **Delivery:** at-least-once. The command **handler** MUST be idempotent,
  keyed on the caller-supplied `idempotency_key` — so a redelivered command
  dedups, while two genuinely distinct commands (e.g. two real rebalances) do
  not.
- **Dedup:** `idempotency_key` is caller-supplied, NOT equal to `event_id`.
- **Ordering:** per `partition_key`; commands targeting the same aggregate MUST
  share a `partition_key` so they are applied in submission order.
- **Mandatory outcome:** every COMMAND MUST produce exactly one outcome FACT
  (schema from EVT-12) with status `ACCEPTED` | `REJECTED` | `EXECUTED` |
  `FAILED`, a reason, and `causation_id` set to the command's `event_id`. A
  command with no outcome event is a defect — it is unobservable.
- **Consumer obligations:** validate authority before acting; emit the outcome
  FACT even on rejection or failure; never treat a COMMAND as a FACT.

## 3. STATE_SNAPSHOT

A STATE_SNAPSHOT is a full materialized state of an aggregate at a point in
time. It lets consumers and replays bootstrap without reading the log from
genesis.

- **Delivery:** at-least-once.
- **Ordering:** latest-wins per aggregate, by `event_time`. An older snapshot
  is discarded if a newer one for the same aggregate has been seen.
- **Log position:** a snapshot MUST reference, in its payload, the durable-log
  position it was taken at — so a consumer can apply the snapshot and then
  resume the log from exactly that point with no gap and no double-application.
- **Immutability:** snapshots are never mutated; a new snapshot supersedes.
- **Retention:** compacted — only the latest snapshot per aggregate is retained.
- **Consumer obligations:** on cold start, load the latest snapshot, then
  resume the log from its referenced position; apply idempotently.

## 4. OBSERVATION

An OBSERVATION carries metrics, model-decision logs, and data-quality signals.

- **Delivery:** at-least-once, **lossy-tolerant under extreme load** — it is
  acceptable to shed OBSERVATIONs to protect the live path.
- **Exception — data-quality observations** (gap, staleness, drift; schemas
  from EVT-14, emitted by DATA-07): these are **not** lossy-tolerant. They get
  FACT-grade delivery and retention, because losing a data-integrity signal
  means operating blind. They carry `EVENT_CLASS_OBSERVATION`, but the
  data-integrity layer MUST NOT shed them.
- **Ordering:** none required; consumers take latest-wins by `event_time`.
- **Retention:** short-retention, NATS-only for ephemeral metrics; data-quality
  observations follow the durable tier.
- **Consumer obligations:** never let OBSERVATION processing block or
  backpressure a FACT/COMMAND path.

## 5. Cross-cutting rules

- **Exactly-once is not a primitive.** Every class uses at-least-once delivery;
  effectively-once is achieved by idempotent consumers + dedup keys.
- **Replayed events** carry `QUALITY_FLAG_REPLAYED` regardless of class. Live
  sinks MUST hard-reject them; only replay-scoped consumers accept them.
- **`event_time` is authoritative** for any consumer doing windowing, model
  input, or backtesting — never processing time.
- **Poison events** (fail deserialization or validation, or exceed the retry
  bound) go to the DLQ for their subject; they never block a partition and are
  never silently dropped.
