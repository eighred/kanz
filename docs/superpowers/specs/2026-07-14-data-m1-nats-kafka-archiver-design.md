# DATA-M1 — The NATS→Kafka Archiver

**Date:** 2026-07-14
**Status:** Approved, not yet implemented
**Board:** `KANZ_TASKS.md` → TODO → DATA-M1
**Architectural decision:** `KANZ_BRAIN.md` §Event platform — "Kafka is DERIVED from NATS by ONE archiver. Services never dual-publish."

---

## 1. The problem

`KANZ_BRAIN.md` calls Kafka the durable log of record and states that DR rebuilds
NATS from the Kafka log. Verified against the code on 2026-07-14:

> **`bus.DialKafka` has exactly ONE non-test caller in the entire repository —
> `services/lake-sink`, and it SUBSCRIBES. Every other service dials NATS and
> only NATS. Nothing has ever written an event to Kafka.**

The broker (`infra/kafka/kafka.yaml`, 3-node KRaft, RF3/min-ISR2), the
provisioned topics, the per-tenant ACLs, the MirrorMaker2 DR replication and the
whole of `lake-sink` are built and waiting on a producer that was never written.

Consequences, each independently verified:

- **History evaporates on a rolling basis.** `EXECUTION` (which carries `order.>`)
  has a 24h max-age and nothing drains it. Realized P&L since inception, the audit
  trail, the regulatory record and every fill older than yesterday are retained
  **nowhere** — `position_fills` holds fill_ids only, for the exactly-once claim,
  not the fills.
- **DR is inert.** It cannot "rebuild NATS from the Kafka log" because the log is empty.
- **Replay tooling (EVT-20) has nothing to read.**
- **`internal/integrity` (DATA-05) has zero importers** outside its own tests — it
  reconciles two transports and the second one was never written to.

### 1.1 The topic topology is almost entirely fictional

Extracting every `{domain}.{entity}` the code actually publishes on — Go via the
same AST walker `TestEverySubjectIsCarriedByAStream` uses, **and `kanz-py`, which
that walker cannot see** — and comparing it to the 13 topics provisioned in
`infra/kafka/topics-job.yaml`:

| Real `{domain}.{entity}` in code (16) | Kafka topic exists? |
|---|---|
| `risk.portfolio`, `inference.feature` (Go); `inference.prediction`, `data.feature` (**kanz-py**) | **Yes** (4) |
| `order.order`, `strategy.signal`, `accounting.balance`, `settlement.instruction`, `compliance.breach`, `compliance.mandate`, `risk.position`, `market.book`, `market.crypto`, `platform.authz`, `platform.compliance`, `platform.mode` | **No** (12) |

Conversely, 9 of the 13 provisioned topics (`execution.order`, `market.equity`,
`market.option`, `data.market_stream`, `observability.model`, `platform.model`,
`platform.config`, and the `.snapshot` siblings) correspond to **no subject any
service publishes**. They were written from the taxonomy doc's *examples* rather
than from the code.

**The cross-language half of this matters more than the count.** `kanz-py`
publishes `inference.prediction.scored` (`kanz_inference/publish.py`) and
`data.feature.drift_detected` (`governance/drift_trigger.py`). A guard that walks
only the Go AST would pronounce the topology safe while being structurally blind
to every subject the Python side publishes — which is the *identical* failure mode
AI-M1 uncovered, where Go required `tenant_id` on the live path and the Python
producer had no tenant concept at all, so nothing `kanz-py` published had ever
been consumable. The guard must cover both languages or it is theatre.

Kafka auto-create is disabled ("topics exist only if provisioned here"), so an
archiver shipped against today's topology would fail closed on essentially the
entire money path: orders, fills, accounting, settlement, mandates, positions.

**Therefore DATA-M1 is two things: the archiver, and the topic topology rebuilt
from ground truth with an arch test that keeps it there.**

---

## 2. Scope

**In:** every subject the code declares, except market ticks and observability.
Concretely, the NATS streams `RISK`, `EXECUTION` (`execution.>`, `strategy.>`,
`order.>`), `INFERENCE` (including `inference.prediction.*`, published by
`kanz-py`), `PLATFORM`, `DATA` (`data.feature.*`, published by `kanz-py`),
`ACCOUNTING`, `COMPLIANCE`, `SETTLEMENT`, `MANDATE`, `POSITION`.

**Out, deliberately:**

- **`MARKET` (`market.book`, `market.crypto`).** Highest volume by a wide margin,
  and re-fetchable from the venue on reconnect — unlike a fill, which is gone
  forever. Archiving ticks is a storage-and-cost decision that deserves its own
  conversation (and it is the natural foundation for the AI brief's tick/L2
  persistence); smuggling it into DATA-M1 would put the highest-volume stream on
  the archiver's critical path, where falling behind is the main failure mode.
- **`OBSERVABILITY`** — a 1h ephemeral stream. Nothing declares a subject on it.

**Also out:** wiring `internal/integrity`. Once Kafka is *derived* from NATS by a
single archiver, the two are no longer independent transports and the detector's
premise no longer holds. Re-evaluate after this ships; deleting it may be the
honest outcome.

---

## 3. Design

### 3.1 Service

`services/archiver`, following the existing service layout (`cmd/archiver`,
`internal/config`, `internal/archive`, `internal/topic`).

**Per-tenant deployment** (`ARCHIVER_TENANT`; **refuses to start if unset**),
matching the convention tv-sync, the OMS and the risk engine already follow. This
is not a preference: NATS isolation is by *account* (the client's SVID maps to a
per-tenant account), so a cross-tenant archiver could not see a tenant's subjects
in the first place.

### 3.2 The mapping (`internal/topic`)

One small pure unit — `Topic(env *envelopepb.Envelope) (string, error)` — derived
from the envelope's **`event_type`**, not the NATS subject. `event_type` carries
the full three-segment logical name and the bus client already enforces
`domain == first segment`, so it is the authoritative name; the subject is a
transport detail.

| Input | Topic |
|---|---|
| `event_type = order.order.submitted`, tenant `__system__`/empty | `order.order` |
| `event_type = order.order.submitted`, tenant `acme` | `acme.order.order` |
| `event_class = STATE_SNAPSHOT` | `…{domain}.{entity}.snapshot` (compacted sibling) |

**Every other case is an error, and an error means NACK, never a guess:**

- reserved leading segment (`replay.`, `dlq.`),
- an `event_type` that is not exactly three segments,
- `env.tenant_id` disagreeing with `ARCHIVER_TENANT` (a cross-tenant leak),
- an unprovisioned tenant — the produce fails because the topic does not exist,
  and that surfaces loudly rather than being cross-filed into the shared
  `__system__` topic, which would breach the Kafka PREFIXED-ACL isolation boundary.

### 3.3 Data flow

One durable NATS consumer per in-scope stream. For each event:

1. Map the envelope to a topic (§3.2). On error: NACK, increment a failure metric, log loudly.
2. Produce to Kafka as `bus.Message{Subject: topic, Key: env.partition_key, Body: <the envelope bytes, verbatim>, Headers: {Kanz-Event-Id: env.event_id}}`.
   `bus.Message.Key` is already used verbatim as the Kafka message key, so
   **per-entity total ordering falls out of the existing bus contract** — events
   sharing a `partition_key` land on one partition, in order.
3. **Ack NATS only after Kafka acknowledges the write.** A failed produce NACKs
   and is redelivered.

The body is archived **verbatim** — the archiver never re-stamps, re-validates or
re-serializes an envelope. It is a transport, not a producer; a component that
rewrites the log of record on its way into the log of record cannot be trusted as
a record of what happened.

**Delivery semantics: at-least-once.** A redelivery after a successful produce
whose ack was lost writes a duplicate. `event_id` is the dedup key, carried as a
Kafka header so a consumer can dedup without parsing the payload.

### 3.4 Availability and ordering

`replicas: 1`, `strategy: Recreate`.

This is superficially the anti-pattern EXEC-M22 deleted, and it is the *opposite*
one. For an **ingress**, a gap is permanent signal loss — the alert arrives, hits
nothing, and is never retried. For an **archiver**, a gap is deferred work: NATS
retains the events (24h; 168h on the money streams) and the pod catches up on
restart. Single writer is also what makes §3.3's ordering correct by construction
— two pods producing concurrently for one partition key can invert order in Kafka,
and a reordered log rebuilds a *different* book downstream, which is a subtler
failure than losing it.

**The real failure mode is not downtime, it is falling behind.** Archiver lag
(NATS consumer pending, per stream) is therefore a first-class metric with an
alert that fires long before lag approaches the stream's max-age. An archiver
silently falling behind its stream's retention is data loss with a delay on it.

Scale-out, if throughput ever demands it, is by sharding whole streams across
deployments — never by adding replicas to one stream's consumer.

### 3.5 The topic topology

`infra/kafka/topics-job.yaml` is rebuilt from ground truth: the 12 missing topics
added, the 9 fictional ones retired, and the 4 that are already correct
(`risk.portfolio`, `inference.feature`, `inference.prediction`, `data.feature`)
left alone. Shape follows the subject's own semantics:

- **State, not events → compacted**, keyed by `partition_key`: `compliance.mandate`
  and `risk.position`. Their NATS streams already recognize this
  (`--max-msgs-per-subject=1`, no max-age); Kafka must not be the place where a
  mandate ages out and a restart comes back ungoverned.
- **Events → time retention**, with the money and obligation topics (`order.order`,
  `accounting.balance`, `settlement.instruction`, `compliance.breach`,
  `strategy.signal`, `risk.portfolio`) at 30d and a paired `dlq.{name}`.
- Permanence is **DATA-M2's** job, not a retention setting here: 30 days is what
  the archiver buys. The lakehouse is what makes history permanent.

### 3.6 The arch test (the durable deliverable)

`TestEverySubjectHasAKafkaTopic` in `test/arch`: every `{domain}.{entity}` the
code publishes on must have a provisioned Kafka topic in `topics-job.yaml`, or the
build fails.

**It must cover both languages.** Reuse the existing `declaredSubjects` AST walker
for Go, and extend subject discovery over `kanz-py` (a scan for three-segment
subject literals — the Python side has no AST walker today and does not need one
for this). A Go-only guard is blind to `inference.prediction` and `data.feature`
by construction, and blindness that reports success is worse than no guard at all:
it is how the topology drifted this far while the NATS side stayed correct.

The archiver fixes today's gap. **This test is what prevents the subject topology
and the topic topology from silently drifting apart again — which is exactly how
we got here.** It is the direct analogue of `TestEverySubjectIsCarriedByAStream`,
which is the only reason the NATS side is correct today.

The reverse direction is deliberately **not** asserted: a provisioned topic with no
publisher is dead weight, not a fault, and failing the build on one would block
provisioning a topic ahead of the code that fills it. The 9 fictional topics are
retired by hand in this task, once.

---

## 4. Testing

**Unit (`internal/topic`).** Snapshot routing, tenant prefixing, `__system__`
un-prefixed, reserved-prefix rejection, malformed `event_type`, cross-tenant
mismatch. Each error path asserts an error, because each one is a fail-closed.

**Integration — against a real NATS and a real Kafka** (the pattern exists:
`pkg/bus/kafka_integration_test.go`, and EXEC-M22's Redis service container in CI):

1. **The payoff test.** Publish events across several streams and partition keys;
   assert each lands in the correct topic, and that events sharing a partition key
   arrive **in order**.
2. **Kafka outage.** Kill Kafka mid-flight; assert nothing is acked on NATS,
   nothing is lost, and the archiver catches up cleanly with no gap when Kafka
   returns.
3. **The red twin, kept executable.** Ack-before-produce **must lose events** under
   test 2. That is the bug this design exists to prevent, and a test that cannot
   reproduce it is not guarding anything.
4. **Fail-closed.** An event for an unprovisioned tenant NACKs; it does not land in
   the `__system__` topic.

**Mutation check.** Remove the ordering key (produce with a nil `Key`) and the
ordering assertion in test 1 must fail. A test that passes without the mechanism
it claims to test is decoration.

---

## 5. Deliverables

| Deliverable | Path |
|---|---|
| Archiver service | `kanz/services/archiver/` |
| Topic mapping + unit tests | `kanz/services/archiver/internal/topic/` |
| Integration tests (real NATS + Kafka) | `kanz/services/archiver/internal/archive/` |
| Arch test | `kanz/test/arch/` — `TestEverySubjectHasAKafkaTopic` |
| Kafka topology rebuilt from ground truth | `kanz/infra/kafka/topics-job.yaml` |
| Deployment (replicas 1, Recreate, Vault CSI, NetworkPolicy: NATS + Kafka only) | `kanz/infra/deploy/archiver-deploy.yaml` |
| Container image + CI (build, vet, test; Kafka service container) | `kanz-py`-style Dockerfile pattern; `.github/workflows/` |

## 6. Done means

- The archiver runs, and every in-scope subject lands in its Kafka topic, in
  per-key order, acked only after Kafka has the write.
- `TestEverySubjectHasAKafkaTopic` passes and **fails** when a subject is added
  without a topic (verified by mutation).
- The Kafka-outage test proves no loss and clean catch-up; its red twin proves the
  ack-before-produce bug is real.
- Archiver lag is exported and alertable.
- `KANZ_BRAIN.md`'s "NOT YET BUILT" flag on the archiver decision is retired.

## 7. Out of scope / follow-ons

- **DATA-M2** — deploy `lake-sink`; the lakehouse is what makes history permanent.
- **Market-tick archival** — its own capacity and cost conversation (§2).
- **`internal/integrity` (DATA-05)** — re-evaluate, possibly delete (§2).
