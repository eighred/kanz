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
| `market` | Market data ingestion | `@eighred/market-data` |
| `risk` | Risk engine | `@eighred/risk-engine` |
| `execution` | Command / order execution | `@eighred/execution` |
| `inference` | Prediction layer (features, predictions) | `@eighred/quant-ml` |
| `platform` | System lifecycle, config, mode | `@eighred/platform` |
| `data` | Data-integrity signals | `@eighred/observability` |
| `observability` | Metrics, traces, decision logs | `@eighred/observability` |

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

### Tenant routing prefix (MT-01c)

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

### Tenant FACT return prefix — `tfact.{tenant}.` (#668)

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

## 7. Governance

- Every logical name is registered in `kanz-schemas` alongside its payload
  schema. Ad-hoc subject creation is not permitted.
- CI (EVT-07) enforces the three-segment rule, `lower_snake_case`, and that the
  `domain` segment is in the §2 closed set.
- The `domain` prefix is the ownership and access-control boundary; it maps to
  a CODEOWNERS team (§2).
- Adding an `entity` or `event_type` is a normal reviewed change; adding a
  `domain` requires architecture review.
