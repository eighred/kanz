# kanz-schemas

The wire contract for the Kanz platform: protobuf definitions under `proto/`,
generated SDKs into `gen/` (Go and Python; **never committed**), and the five
normative specifications below.

```sh
buf generate    # regenerate the Go/Python SDKs
buf lint
buf breaking --against '.git#branch=main'
```

## What is normative here

These five specifications are not commentary on the code — they are the contract
the code implements, and they are cited by section number from Go, Python,
TypeScript, SQL and the `.proto` files themselves. A citation reads
`kanz-schemas/README.md § Subject Taxonomy §4`: the first `§` names the
specification, the second is that specification's own section number, unchanged
from when each was a separate file under `kanz-schemas/docs/` (retired
2026-08-26 — the repository keeps persistent documentation in README files, and
this module had none).

| Specification | Governs |
|---|---|
| [Subject Taxonomy](#subject-taxonomy) | the logical name of every event stream, and its NATS/Kafka mapping |
| [Envelope Policy](#envelope-policy) | what may live in the Envelope, and how `envelope_version` moves |
| [Event Class Rules](#event-class-rules) | FACT / COMMAND / STATE_SNAPSHOT / OBSERVATION semantics |
| [Schema Evolution](#schema-evolution) | when a payload change is breaking, and what a producer must then do |
| [Inference Degraded-Mode Contract](#inference-degraded-mode-contract) | how a prediction consumer behaves when inference is unavailable, slow, or unsure |

**The first three are one-way doors.** A logical name that has been published
to is never renamed or repurposed, a retired field number is never reused, and
an envelope field once required stays required. Read the relevant specification
before changing a `.proto` file; `buf breaking` catches the wire-level half of a
mistake here, and nothing catches the rest.

---

## Subject Taxonomy

Defines the broker-independent logical naming of every event stream in Kanz and
its mapping onto NATS subjects and Kafka topics. The logical name is the stable
contract; broker mappings are implementation.

### 1. Logical name

Every event stream has a three-segment logical name:

```
{domain}.{entity}.{event_type}
```

- **domain** — the owning bounded context. Drawn from the fixed list in §2.
- **entity** — the thing within that domain the event concerns
  (e.g. `equity`, `option`, `portfolio`, `order`, `model`, `feature`).
- **event_type** — what happened. Past-tense for FACTs (`trade`, `scored`,
  `exposure_recomputed`); noun/imperative for COMMANDs (`command`).

Rules:

- Exactly three segments. No more, no fewer.
- Each segment is `lower_snake_case`, ASCII, non-empty.
- The name carries **no version suffix** — payload versioning is the envelope's
  `schema_version`.
- A logical name, once published to, is never renamed or repurposed (same
  one-way-door discipline as schemas).

Examples:

```
market.equity.trade
market.equity.quote
market.option.trade
risk.portfolio.exposure_recomputed
execution.order.command
execution.order.command_outcome
inference.feature.computed
inference.prediction.scored
platform.model.deployed
platform.config.changed
data.market_stream.gap_detected
data.feature.drift_detected
observability.model.decision_logged
```

### 2. Domains

The `domain` segment is a closed set. Adding a domain requires architecture
review (it implies a new bounded context).

| Domain | Bounded context | CODEOWNERS team |
|---|---|---|
| `market` | Market data ingestion | `@eighred/market-data` |
| `risk` | Risk engine | `@eighred/risk-engine` |
| `execution` | Command / order execution | `@eighred/execution` |
| `inference` | Prediction layer (features, predictions) | `@eighred/quant-ml` |
| `platform` | System lifecycle, config, mode | `@eighred/platform` |
| `data` | Data-integrity signals | `@eighred/observability` |
| `observability` | Metrics, traces, decision logs | `@eighred/observability` |

### 3. Relationship to the Envelope

- The envelope `event_type` field carries the **full three-segment name**
  (e.g. `market.equity.trade`).
- The envelope `domain` field carries the **first segment only**
  (e.g. `market`).
- These must agree: `domain` == first segment of `event_type`. The client
  library enforces this on publish.

### 4. NATS mapping (live spine)

NATS subjects use the logical name verbatim — dots are NATS's native hierarchy
separator:

```
subject = {domain}.{entity}.{event_type}
```

Subscription patterns use NATS wildcards:

- `market.>` — every market event
- `market.equity.>` — every equity event
- `risk.portfolio.*` — every event_type for `risk.portfolio`

JetStream streams bind subject ranges and own retention:

- One stream per `{domain}` by default (e.g. stream `MARKET` binds `market.>`),
  giving a per-domain retention boundary.
- Split to per-`{domain}.{entity}` streams when an entity needs a different
  retention or replication policy than its domain siblings.

### 5. Kafka mapping (durable log)

Kafka has no hierarchy, so the logical name is flattened:

```
topic = {domain}.{entity}
```

- `event_type` is **discriminated within the topic** via the envelope
  `event_type` field — sibling event types of one entity stay co-located and
  co-ordered.
- Topic partition key = envelope `partition_key`. Events sharing a
  `partition_key` land on one partition and are totally ordered.
- Retention and compaction are set **per topic** (i.e. per `{domain}.{entity}`).
- **STATE_SNAPSHOT exception:** snapshots need log compaction while sibling
  FACTs need time-retention, so snapshots go to a separate compacted topic
  `{domain}.{entity}.snapshot`.

### 6. Reserved prefixes

Two top-level prefixes are reserved and are **not** part of the
`{domain}.{entity}.{event_type}` space:

- `replay.` — replay namespace, `replay.{run_id}.{original_name}` (EVT-20).
  Live sinks reject anything under `replay.`.
- `dlq.` — dead-letter, `dlq.{original_name}` for poison events.

No domain may be named `replay` or `dlq`.

#### Tenant routing prefix (MT-01c)

Tenant is an **isolation boundary, not part of the logical name** — the logical
name stays the stable 3-segment contract above. How the tenant manifests at the
broker is broker-specific and keys on the envelope `tenant_id`:

- **NATS** — isolation is by **account** (one per tenant), not by subject. The
  client's SVID maps to a per-tenant account (`tls.verify_and_map`), so the
  subject string is the unchanged 3-segment name and a tenant simply cannot see
  another's `{domain}.>`.

  **The one exception, and why it is not really one (MT-02, #358).** That model
  works for a workload that BELONGS to a tenant. It breaks at a SHARED one: the
  api-gateway is the sole front door and holds a single SVID, therefore a single
  account, and NATS routes on subject — it cannot dispatch on the envelope's
  `tenant_id`. A publish in `__system__` reaches nothing in a tenant account;
  measured, with a same-account control.

  So a cross-tenant publisher prefixes the WIRE subject with
  `tenant.{tenant}.` — and that prefix exists **only inside `__system__`**. Each
  tenant account imports its own prefix with a `to:` remap, and `__system__` maps
  its own back to itself, so by the time any workload sees the message the
  subject is the unchanged 3-segment name. `event_type` never carries the prefix.
  The contract above therefore holds everywhere it is read: inside a tenant
  account, and in every envelope.
- **Kafka** — has no account concept, so the tenant is a literal **topic
  prefix**: `{tenant}.{domain}.{entity}`, confined by a PREFIXED ACL on
  `{tenant}.`. `{tenant}` joins `replay`/`dlq` as a reserved leading segment;
  no domain may be named like a tenant.

#### Tenant FACT return prefix — `tfact.{tenant}.` (#668)

`tenant.{tenant}.` above carries **commands INTO** a tenant account. It is
one-directional, and for a long time nothing carried anything back: NATS
accounts are isolated by construction, so every FACT a tenant's OMS published
was visible to no platform service. A tenant got orders in and nothing out —
no ledger, no positions, no audit trail — with every service Ready
throughout, because *"this tenant produced no events"* and *"this tenant's
events cannot reach me"* are the same observable state.

The return path is the mirror image: each tenant account **exports** its FACT
subject spaces, and `__system__` **imports** them under `tfact.{tenant}.`. The
prefix exists only inside `__system__`; nothing in a tenant account ever sees
it, so the 3-segment contract above still holds everywhere it is read, and
`event_type` never carries it.

**Why a second prefix rather than reusing `tenant.`.** The inbound bridge
already owns `tenant.*.order.>` — that is the `TENANT_ORDER` stream's subject
set — and two JetStream streams may not claim overlapping subjects. Importing
a tenant's order FACTs under the same prefix would land them inside the
*command* stream, sharing its 24h retention, with the direction of travel
invisible in the subject. `tfact.` says outbound in the name and leaves the
working command bridge untouched. It is bound by the `TENANT_FACT` stream at
168h.

`tfact` joins `tenant`/`replay`/`dlq` as a **reserved leading segment**: no
domain may be named `tfact`.

**It must not be imported unprefixed.** The logical names are what
`__system__`'s own consumers subscribe: the platform archiver would NACK
forever (`internal/topic.For` refuses an envelope whose `tenant_id` is not its
own) and platform accounting would fold another tenant's event into
`__system__`'s book (#223). An unprefixed import is worse than none.

**Exporting a FACT space does not loop the commands back out.** A tenant's
commands arrive on `order.order.submit` *inside* its account — the same space
it exports — so a re-export would return every command to the platform as
though the tenant had emitted it, double-counting the audit trail. NATS does
not re-export what arrived by import; verified against nats-server v2.14.5 with
both halves of the real `tenancy.yaml` loaded.

The reserved tenant `__system__` carries cross-cutting platform/observability
streams and pre-tenancy (untenanted) events; in Kafka those keep the
**un-prefixed** legacy topic names. Provisioning lives in `kanz/infra/{nats,
kafka}/tenancy.yaml`.

### 7. Governance

- Every logical name is registered in `kanz-schemas` alongside its payload
  schema. Ad-hoc subject creation is not permitted.
- CI (EVT-07) enforces the three-segment rule, `lower_snake_case`, and that the
  `domain` segment is in the §2 closed set.
- The `domain` prefix is the ownership and access-control boundary; it maps to
  a CODEOWNERS team (§2).
- Adding an `entity` or `event_type` is a normal reviewed change; adding a
  `domain` requires architecture review.

**Subject / Topic Taxonomy.** 
---

## Envelope Policy

Governs the `Envelope` message (`proto/envelope/v1/envelope.proto`) and the
`envelope_version` field. Payload schema evolution is governed separately by
[Schema Evolution](#schema-evolution) (EVT-06).

### 1. The constitution principle

The `Envelope` is the universal wrapper on every event crossing the Kanz
backbone. Every producer stamps it; every consumer, every piece of generic
infrastructure tooling, and every replay depends on it. It cannot be migrated
cheaply once events carrying it exist in the durable log.

Therefore the Envelope is treated as a constitution:

- **Additive-only, forever.** No field is ever removed, renumbered, retyped, or
  given new semantics.
- **Breaking changes are structurally impossible** within the `Envelope`
  message. There is no process for a breaking envelope change because there is
  no acceptable one — see §6.
- Changes are rare, deliberate, and require architecture-level review (§5).

### 2. Field-inclusion bar

A field may be added to the Envelope only if it passes **at least one** of
these two tests:

- **Universality** — every event class (FACT, COMMAND, STATE_SNAPSHOT,
  OBSERVATION), in every domain, meaningfully carries it.
- **Payload-blind tooling** — generic infrastructure (schema registry, data
  lake, observability, replay tooling, routing) needs it to do its job
  *without deserializing the payload*.

If a field passes neither test, it belongs in a payload schema, not the
Envelope.

### 3. What does not belong in the Envelope

Reject a proposed field if it is any of:

- **Family-specific** — meaningful only to one event family (e.g. a price, a
  model version, a portfolio id). That is payload.
- **Derivable** — computable from other envelope fields or the payload.
- **Consumer state** — a consumer's processing status, offsets, or
  acknowledgements. The Envelope describes the event, not its handling.
- **Mutable** — anything that would need to change after publish. Envelope
  fields are immutable once stamped.

### 4. Field numbering discipline

- Field numbers are **never reused and never renumbered.**
- A retired field's number *and* name are moved to a `reserved` declaration,
  permanently.
- Numbers 1–15 (single-byte wire tags) are fully allocated by envelope v1. New
  fields take 2-byte tags; this is acceptable given hot-path events are batched
  (one Envelope amortized over N payloads).
- Protobuf's reserved range 19000–19999 is never used.

### 5. Change process

Every change to `envelope.proto` requires:

1. A written proposal stating which §2 test the field passes and why §3 does
   not exclude it.
2. Approval from `@eighred/architecture` (enforced by CODEOWNERS on
   `/proto/envelope/`).
3. A passing `buf breaking` check (EVT-07) — which mechanically blocks any
   non-additive change.
4. An `envelope_version` increment in the same change (§6).

### 6. `envelope_version` semantics

`envelope_version` is a monotonic `uint32` carried in every Envelope.

- It is incremented by exactly 1 each time one or more fields are added to the
  Envelope.
- It records the highest Envelope schema version the producer was built
  against. A consumer reading an Envelope with a version higher than it knows
  simply ignores the unknown fields (proto3 forward compatibility) — a higher
  version is *always* safe to receive.
- A bump **never** signals a breaking change. Because the Envelope is
  additive-only, every version N is wire- and semantically-compatible with
  every version < N.
- It is **not** the payload version. Payload versioning is `schema_version`,
  governed by [Schema Evolution](#schema-evolution).

If a breaking envelope change were ever genuinely unavoidable, it would not be
an `envelope_version` bump — it would require a new `Envelope` message in a new
package version (`envelope.v2`), a dual-write migration across every producer,
every consumer, and the entire durable log, measured in quarters. This is
documented only to be explicit that it is a last resort with no streamlined
path. Design to never need it.

### 7. Presence and value invariants

proto3 cannot mark fields required. The Envelope schema therefore does not
enforce presence — the **shared bus client library** (EVT-17/18/19) validates
every Envelope on the publish path and rejects malformed ones before they reach
the backbone.

Per-field presence requirements (required vs. may-be-empty) are documented
inline in `envelope.proto` and are the authoritative spec the client library
implements.

**Envelope Versioning & Field-Inclusion Policy.** 
---

## Event Class Rules

**Event Class Handling Rules.** Authoritative spec for the per-class consumer obligations of the `EventClass`
enum (`proto/envelope/v1/event_class.proto`). Every `Envelope` carries an
`event_class`; this document defines what a consumer is **permitted and
required** to do for each value.

Enforcement: the shared bus client library (EVT-17/18/19) implements the
publish-side rules; consumer-side rules are a contract every consumer must
honor.

### Summary

| Class | Delivery | Ordering | Dedup key | Mutable | Retention tier |
|---|---|---|---|---|---|
| FACT | at-least-once | per-partition | `event_id` | no | durable log (Kafka) |
| COMMAND | at-least-once | per-partition (per target) | `idempotency_key` (caller-set) | no | durable log (Kafka) |
| STATE_SNAPSHOT | at-least-once | latest-wins per aggregate | `event_id` | no | compacted |
| OBSERVATION | at-least-once, lossy-tolerant* | none required | `event_id` | no | short-retention (NATS) |

\* except data-quality observations — see §4.

### 1. FACT

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

### 2. COMMAND

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

### 3. STATE_SNAPSHOT

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

### 4. OBSERVATION

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

### 5. Cross-cutting rules

- **Exactly-once is not a primitive.** Every class uses at-least-once delivery;
  effectively-once is achieved by idempotent consumers + dedup keys.
- **Replayed events** carry `QUALITY_FLAG_REPLAYED` regardless of class. Live
  sinks MUST hard-reject them; only replay-scoped consumers accept them.
- **`event_time` is authoritative** for any consumer doing windowing, model
  input, or backtesting — never processing time.
- **Poison events** (fail deserialization or validation, or exceed the retry
  bound) go to the DLQ for their subject; they never block a partition and are
  never silently dropped.


---

## Schema Evolution

**Payload Schema Evolution Rules.** Governs every **payload** schema under `proto/{domain}/v1/` and the envelope
fields that version it — `schema_version` and `payload_schema_ref`. Envelope
evolution is governed separately by [Envelope Policy](#envelope-policy) (EVT-03); the
envelope is a constitution, payloads are not.

### 1. The core asymmetry

The Envelope is additive-only forever because breaking it is structurally
impossible to migrate. Payload schemas are allowed to break — but a break is
still a one-way door:

- Kafka is the durable log. Events carrying a payload schema outlive the code
  that produced them, for the full retention window of their topic.
- Replay (EVT-20) reads historical events with **current** code.
- Producers and consumers run mixed versions simultaneously during any rollout.

So payload evolution has two modes, and the whole of this doc is about telling
them apart: **non-breaking** changes edit a schema in place; **breaking**
changes cut a new package version and require a dual-write migration.

### 2. Compatibility direction

Because the log is durable and rollouts are mixed-version, every payload schema
must hold **full compatibility** — backward *and* forward — across a single
package version:

- **Backward** — current code reads events written by any older v1 producer.
  Non-negotiable for the entire Kafka retention window, because replay depends
  on it.
- **Forward** — older code reads events written by a newer v1 producer. proto3
  gives this for unknown fields (they are preserved, not dropped); the rule
  exists so we never *rely* on a consumer seeing a new field.

Full compatibility is what makes a non-breaking change safe to ship without
coordinating producers and consumers. A change that cannot preserve it is
breaking, by definition — see §5.

### 3. Field-number discipline

Identical discipline to the Envelope (§ Envelope Policy §4), restated because
it applies to every payload message and enum:

- Field numbers are **never reused and never renumbered.**
- A retired field's number *and* name move to a `reserved` declaration,
  permanently. Same for a retired enum value's number and name.
- New fields take the next free number. Numbers 1–15 (single-byte wire tags)
  should be spent on the highest-frequency fields of hot-path messages.
- The protobuf reserved range 19000–19999 is never used.
- Every enum has an `*_UNSPECIFIED = 0` zero value. Consumers MUST treat an
  unknown enum value as `UNSPECIFIED` — this is what makes §4's "add an enum
  value" non-breaking.

### 4. Non-breaking changes — edit in place, no `schema_version` bump

These preserve full compatibility (§2). `buf breaking` (EVT-07) passes; the
schema is edited in place in `proto/{domain}/v1/`; `schema_version` does **not**
change. Only the registry version in `payload_schema_ref` advances (§6).

- Adding a new field whose zero value is a valid, safe default.
- Adding a new value to an existing enum (consumers already handle unknowns).
- Adding a new message type, or a new field to a nested message under these
  same rules.
- Loosening a value invariant (a previously-rejected value is now accepted).
- Documentation-only changes to comments.

### 5. Breaking changes — new package version + dual-write

A breaking change is **anything `buf breaking` rejects**, plus the semantic
breaks it cannot see:

- Removing, renumbering, or retyping a field. *(buf catches these.)*
- Changing a field's meaning or units — same wire type, different semantics.
- Tightening a value invariant — a previously-valid value is now rejected.
- Promoting an effectively-optional field to effectively-required.

The last three are wire-compatible, so `buf breaking` passes them. They are
caught **only by CODEOWNERS review** — reviewers of `proto/{domain}/` must
check this list, not just trust CI.

Process for a breaking change:

1. The affected message is re-declared in a new package version directory,
   `proto/{domain}/v2/` (`package {domain}.v2`). The `v1` schema is left
   untouched. Unchanged sibling messages are **not** force-migrated.
2. Producers **dual-write** v1 and v2 for the duration of the migration.
3. Consumers migrate to v2 on their own schedule (the durable log lets them).
4. v1 is retired — its topic and `reserved` tombstone kept — only once the
   Kafka retention window has rolled past the last v1 event.

### 6. `schema_version` vs `payload_schema_ref`

The envelope carries two payload-versioning fields; they are not redundant.

- **`schema_version`** (`uint32`) — the **major** payload version. It equals
  the `N` of the message's `{domain}.vN` package. It increments by exactly 1
  on a breaking change (§5) and at no other time. It is per-event-type:
  `market.equity.trade` and `risk.portfolio.exposure_recomputed` advance
  independently. It is the field a consumer branches on to pick a decoder.
- **`payload_schema_ref`** (`string`, `<schema-id>:<version>`) — the
  fine-grained registry pointer (EVT-16). Its version advances on **every**
  schema change, breaking or not, so generic tooling and replay can pin the
  exact schema an event was written against. A non-breaking §4 change moves
  this and nothing else.

A consumer that only needs "can I decode this" reads `schema_version`. A
consumer or tool that needs "exactly which schema" resolves `payload_schema_ref`
against the registry.

### 7. Version-bump triggers — summary

| Change | `buf breaking` | New `vN` package | `schema_version` | `payload_schema_ref` |
|---|---|---|---|---|
| Add field (safe default) | passes | no | unchanged | bump |
| Add enum value | passes | no | unchanged | bump |
| Add message type | passes | no | unchanged | bump |
| Loosen invariant | passes | no | unchanged | bump |
| Doc-only change | passes | no | unchanged | bump |
| Remove / renumber / retype field | **fails** | yes | +1 | new |
| Change semantics or units | passes* | yes | +1 | new |
| Tighten invariant | passes* | yes | +1 | new |
| Optional → required | passes* | yes | +1 | new |

\* wire-compatible but semantically breaking — invisible to `buf breaking`,
caught only by CODEOWNERS review (§5).

### 8. Enforcement

- **`buf breaking`** in CI (EVT-07) mechanically blocks every wire-breaking
  change to a `v1` package against its committed baseline.
- **CODEOWNERS review** on `proto/{domain}/` is the only gate for semantic
  breaks that pass `buf breaking` (§5). Reviewers own that the §7 table was
  applied honestly.
- **The schema registry** (EVT-16) is the runtime source of truth.
  `payload_schema_ref` must resolve there before an event is publishable.
- **Replay determinism** (EVT-21d) depends on §2 holding: replaying an old log
  with current code must produce identical output, which is only true if every
  intervening change was genuinely non-breaking or genuinely a new `vN`.


---

## Inference Degraded-Mode Contract

Governs how the prediction layer behaves when it cannot serve a fresh,
fully-trusted prediction — and what consumers MUST do with the response.

Companion to `proto/inference/v1/prediction.proto` (PRED-01) and the
hybrid streaming + sync-gRPC architecture described in KANZ_BRAIN.

### 1. The core rule

The prediction layer **never returns "no prediction"** under degraded
conditions. It always returns a `PredictionEnvelope` and sets `mode`
honestly — `NORMAL` for the full-trust path, `DEGRADED` for everything
else. The consumer branches on `mode` BEFORE acting.

Black-holing a request (refusing to respond, returning an error) is a
correctness regression: the caller is left blind, can't distinguish "the
model is silent" from "the model said no", and has no signal to fall
back on its own logic. Degraded-with-explicit-flag is always strictly
better than no-response.

### 2. Three degraded triggers

These are the only conditions under which the inference layer SHALL
set `mode = PREDICTION_MODE_DEGRADED`:

#### 2.1 Inference unavailable

The model server is down, the gRPC channel is broken, or the bus
consumer-lag has exceeded the operating budget. The inference layer
returns the **last-known-good prediction** for the subject from its
cache (PRED-04) and sets `degraded_reason` to one of:

- `inference_unavailable` — model server unreachable
- `bus_lag_exceeded` — async path back-pressured beyond budget
- `circuit_open` — sync-path circuit breaker tripped (PRED-07)
- `pool_saturated` — interactive worker pool is at admission limit
  (PRED-08); the request was admit-rejected rather than queued
  indefinitely

If no cache entry exists for the subject (cold cache, first-ever
request), the layer returns `value = 0`, `confidence = 0`,
`degraded_reason = "no_cached_prediction"`. The consumer treats this as
"no information, do not act."

#### 2.2 Inference slow

Sync gRPC calls exceeding the per-call timeout budget (PRED-07) fall
back to cache rather than blocking the caller indefinitely. The
streaming path's equivalent is consumer-lag scaling (PRED-14) —
slowness routes back to 2.1 once the lag exceeds budget.

`degraded_reason = "inference_timeout"` with the timeout value in
the explanation map (e.g. `{"budget_ms": "50"}`).

#### 2.3 Low confidence

The model produced a prediction but its calibrated confidence is below
the per-model threshold. The threshold is the model's contract (set in
the model artifact metadata, PRED-09), not a global constant — a
high-conviction signal model may set 0.8; a probability-of-default
model may set 0.3.

`degraded_reason = "low_confidence"`; `confidence` field carries the
actual value. A model that does NOT expose calibrated confidence is
treated as ALWAYS low-confidence — sets `confidence = 0` and `mode =
DEGRADED` on every response (see §1 — "unknown confidence" must register
as "no confidence", not be silently mapped to high).

### 3. Producer obligations

The inference layer (PRED-04 / PRED-08) MUST:

- Always return a `PredictionEnvelope` — never black-hole.
- Set `mode` honestly: `DEGRADED` if any §2 trigger fires, `NORMAL`
  otherwise. `UNSPECIFIED` is never set by a producer.
- Set `degraded_reason` to a stable identifier from §2 (machine-
  readable; downstream alerts match on it). Free-form context goes in
  `explanation` as a sidecar map, not in `degraded_reason`.
- Emit one `observation.v1.DecisionLog` per degraded response with the
  same `degraded_reason` so the audit trail captures it independently
  of whether the consumer logged it.
- For the streaming path, set the envelope `quality_flags` to include
  `QUALITY_FLAG_DEGRADED` when `mode == DEGRADED` — so payload-blind
  observability tooling sees the signal without parsing the payload.

### 4. Consumer obligations

Consumers of `PredictionEnvelope` MUST:

- Branch on `mode` BEFORE reading `value`. A consumer that ignores
  `mode` and acts on the `value` field is using degraded predictions as
  if they were fresh — a correctness bug.
- Have a documented fallback for `mode == DEGRADED` per use case. A
  hot-path consumer may refuse to trade on degraded signals; a
  reporting consumer may surface them with a UI flag; a backtest
  consumer may ignore them entirely.
- Treat `mode == UNSPECIFIED` as `DEGRADED` (defensive — a producer
  that fails to set `mode` is broken, but the consumer should not
  silently treat the response as full-trust).
- Never elevate a `DEGRADED` response to `NORMAL` based on the value
  field alone. The producer made the trust call; consumers don't
  override it.

### 5. Mode is per-response, not per-subject

A subject can receive `NORMAL` predictions and `DEGRADED` ones
interleaved — the inference layer makes the trust call PER request,
not per subject. A model that just rolled back may serve some
subjects normally (cached predictions from the previous version are
still trustworthy) and others degraded (no cache for newly-active
subjects). Consumers must not assume "this subject is in degraded
mode" — that state is per-response.

### 6. Cross-path uniformity

Both the streaming path (bus consumer) and the sync gRPC path (PRED-06
/ PRED-07) follow this contract identically. A streaming consumer and a
sync caller seeing the same subject at the same as_of MUST observe the
same `mode` + `degraded_reason` — the trust decision is made by the
inference layer, not the transport.

The implementation reuses one decision function across both paths
(PRED-04 + PRED-07 share the same trigger logic). Divergence between
the two would let a consumer route around degraded mode by switching
paths, defeating the contract.

### 7. What this doc does NOT govern

- Specific timeout values, cache TTLs, or threshold choices — those are
  per-deployment operational tuning, not contract.
- Feature-staleness signalling — features have their own freshness
  conventions (DATA-02/03), the prediction layer reads them but does
  not redefine them.
- Model-version pinning or shadow-traffic policy — those are PRED-09
  and PRED-10 concerns.
- The risk engine's `DEGRADED` mode (RISK-11). Prediction and risk
  degraded modes are independent — a risk query can be NORMAL even
  while its inputs include DEGRADED predictions, and vice versa.
