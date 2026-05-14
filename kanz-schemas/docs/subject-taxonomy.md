# Subject / Topic Taxonomy

Defines the broker-independent logical naming of every event stream in Kanz and
its mapping onto NATS subjects and Kafka topics. The logical name is the stable
contract; broker mappings are implementation.

## 1. Logical name

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

## 2. Domains

The `domain` segment is a closed set. Adding a domain requires architecture
review (it implies a new bounded context).

| Domain | Bounded context | CODEOWNERS team |
|---|---|---|
| `market` | Market data ingestion | `@kanz-eng/market-data` |
| `risk` | Risk engine | `@kanz-eng/risk-engine` |
| `execution` | Command / order execution | `@kanz-eng/execution` |
| `inference` | Prediction layer (features, predictions) | `@kanz-eng/quant-ml` |
| `platform` | System lifecycle, config, mode | `@kanz-eng/platform` |
| `data` | Data-integrity signals | `@kanz-eng/observability` |
| `observability` | Metrics, traces, decision logs | `@kanz-eng/observability` |

## 3. Relationship to the Envelope

- The envelope `event_type` field carries the **full three-segment name**
  (e.g. `market.equity.trade`).
- The envelope `domain` field carries the **first segment only**
  (e.g. `market`).
- These must agree: `domain` == first segment of `event_type`. The client
  library enforces this on publish.

## 4. NATS mapping (live spine)

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

## 5. Kafka mapping (durable log)

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

## 6. Reserved prefixes

Two top-level prefixes are reserved and are **not** part of the
`{domain}.{entity}.{event_type}` space:

- `replay.` — replay namespace, `replay.{run_id}.{original_name}` (EVT-20).
  Live sinks reject anything under `replay.`.
- `dlq.` — dead-letter, `dlq.{original_name}` for poison events.

No domain may be named `replay` or `dlq`.

## 7. Governance

- Every logical name is registered in `kanz-schemas` alongside its payload
  schema. Ad-hoc subject creation is not permitted.
- CI (EVT-07) enforces the three-segment rule, `lower_snake_case`, and that the
  `domain` segment is in the §2 closed set.
- The `domain` prefix is the ownership and access-control boundary; it maps to
  a CODEOWNERS team (§2).
- Adding an `entity` or `event_type` is a normal reviewed change; adding a
  `domain` requires architecture review.
