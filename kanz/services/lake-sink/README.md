# lake-sink (LAKE-01a)

Schema-evolution-aware CDC sink: reads the derived Kafka archive produced by
the sole NATS archiver and lands accepted events as NDJSON, decoding each
payload against the EVT-16 schema registry so the table columns track the
*published* schema rather than a compiled-in type.

Because the decode is dynamic (registry descriptor → `dynamicpb` → JSON), a field
added additively to a payload schema surfaces in the payload JSON with **no redeploy**
of this service — the LAKE-01a "schema-evolution-aware" property. This is the
analytical-store half of the backtest-ready design (EVT-21d replay determinism +
PRED-11 point-in-time store); LAKE-01b/c build the point-in-time materialization
and backtest harness on top of what lands here.

## Pipeline

```
Kafka topic {domain}.{entity}  ──▶  decode (EVT-16 registry)  ──▶  Sink
   (bus.Consumer, one group)         dynamicpb → JSON              Hive NDJSON
```

| Package | What |
|---|---|
| `internal/decode` | EVT-16 resolve client (`GET /schemas/{ref}` → FileDescriptorSet) + immutable per-ref cache + `dynamicpb`/`protojson` decode to a JSON object |
| `internal/sink` | `Sink` interface + the default `FileSink` (Hive-partitioned NDJSON) |
| `internal/cdc` | bus consumer folding envelope + decoded payload into a `Row`, plus landing/decode metrics |
| `internal/server` | `/healthz` `/readyz` `/metrics` |

## Landing layout

```
{OUTPUT_DIR}/{domain}/{entity}/dt=YYYY-MM-DD/part-{instance}.ndjson
```

One row per event: the stable envelope columns + the decoded `payload` as a
nested object. `entity` is the schema ref's message name
(`market.v1.MarketDataEvent:7` → `MarketDataEvent`), `dt` partitions by
`event_time`. The service requests a generated hostname/PID/random run identity
for part files, keeping event `Source` unchanged. Explicit constructor identifiers
reopen their historical append path and must be distinct among concurrent writers.
Existing files remain in place. Snapshot sealing, catalog commits, object-store
authorization and multi-replica deployment acceptance remain separate work;
the deployed singleton/RWO bound is retained.

## Delivery semantics

At-least-once: the sink may see a row twice (duplicates share `event_id`, resolved
by downstream compaction). Decode outcomes:

- **ok** → row carries the decoded payload.
- **transient** resolve failure (registry down, 5xx) → NAK, the bus redelivers —
  never drop the chance to land the decoded payload.
- **permanent** decode failure (unknown ref, malformed payload) → land the row
  envelope-only with `decode_error` set and **ack**, so a poison message neither
  blocks the partition nor erases that the event occurred.

Every row is flushed (`bufio.Flush` + `fsync`) before the handler acks, so a
row is durable by the time its Kafka offset commits — never buffered past an
ack (DATA-M5). Clean shutdown flushes and closes the sink as a final
backstop, but there is no periodic flush: acking without flushing would be
exactly the bug this sink exists to not have.

## Run

```sh
LAKE_SINK_BROKERS=kafka:9092 \
LAKE_SINK_TOPICS=market.data,risk.portfolio,risk.exposure \
LAKE_SINK_REGISTRY_URL=http://schema-registry:8080 \
LAKE_SINK_OUTPUT_DIR=/var/lake/landing \
go run ./services/lake-sink/cmd/lake-sink
```

Config (env): `LAKE_SINK_LISTEN` (`:8085`), `LAKE_SINK_BROKERS` (required),
`LAKE_SINK_TOPICS` (required), `LAKE_SINK_REGISTRY_URL` (unset ⇒ envelope-only,
payloads undecoded), `LAKE_SINK_OUTPUT_DIR` (required), `LAKE_SINK_CONSUMER_GROUP`
(`lake-sink`), `LAKE_SINK_SOURCE` (`lake-sink`), `LAKE_SINK_OTLP_ENDPOINT`.

Set `SPIFFE_ENDPOINT_SOCKET=unix:///run/spiffe/spire-agent.sock` when using the
workload-identity Kafka listener (port 9094) with the SPIFFE CSI socket mounted.
A configured source must initialize successfully before
Kafka subscriptions start; failure exits nonzero without plaintext fallback.
Leaving the variable unset retains plaintext local development. The rotating
identity source stays open until subscriptions and the Kafka client have closed.

The shared mesh policy accepts valid peer SVIDs in `kanz.internal`; this is not
a Kafka-only peer allowlist. Constructing the Kafka client does not perform a
broker handshake, and readiness does not prove broker authentication. This
wiring does not establish Redpanda URI-SAN principal mapping or ACL equivalence;
those remain production migration acceptance requirements under #1413.

### Coordinated image promotion

The active deployment remains unchanged for this source-first change.
`broker-identity-patch.yaml` is a staged strategic merge patch outside the GitOps
resource directories; do not apply it alone. The existing pinned image does not
consume the new socket configuration, and changing its pod template would
restart the `Recreate` singleton without enabling this wiring.

Before promotion, verify an immutable image digest containing this source and
the target workload registration, CSI availability, broker TLS authorization and
principal/ACL behavior. In one reviewed GitOps change, update the deployment to
that digest and merge the staged socket environment, mount and volume fields.
Keep the existing security context, PVC, replica count and `Recreate` strategy.
Correct the old unauthenticated-listener comment in that same promotion.
Verify authenticated consumption and durable landing after rollout; HTTP
readiness alone is insufficient evidence. Account for the singleton restart and
Kafka retention/catch-up window before scheduling the change.

Rollback restores the previous image digest and its matching pod configuration
together, including removal of the newly added socket fields. It restores the
previous configuration, not proof that the old image can authenticate to the
target broker. Preserve the landing PVC and consumer offsets; do not reset them
to make a failed rollout appear healthy. Record any remaining recovery or
authentication blocker under #1413.

## Tests

`go test -p 1 ./services/lake-sink/...` — decode round-trip over a real
FileDescriptorSet (`common.v1.Decimal`), transient-vs-permanent resolve
classification, immutable-ref caching, Hive partition layout + path-segment
sanitization, and the three CDC decode outcomes (land / ack-envelope-only / NAK).

### Optional raw custody for landed rows

`LAKE_SINK_RETAIN_RAW_FRAME=true` adds `raw_capture` to each successfully
landed NDJSON row. The default is disabled and leaves existing row JSON unchanged.
Capture contains base64 original EventFrame bytes, their SHA-256 and observed Kafka
topic/partition/offset (zero is valid). `source_identity: null` explicitly means
cluster identity, topic UUID and incarnation are unavailable; these are not inferred.
The same row Write/Flush/fsync barrier covers the raw bytes and decoded columns.

This is not complete physical-offset history: normal validation, deduplication,
retry and DLQ behavior remain unchanged. A deduplicated delivery never dispatches;
a successfully parked failure can commit its offset without landing a row. Existing
NDJSON lacks original bytes and cannot be retroactively labeled raw custody. No
catalog transaction, source-incarnation proof, new archiver or financial materializer
is introduced. Received frame/provenance copies are isolated between attempts.

Raw mode accepts original frames up to 16 MiB; larger frames are unsupported by
this opt-in capture mode and follow existing handler retry/DLQ policy. The copy
is taken only after validation and a successful dedup claim. This bound limits
additional custody copies, not Kafka fetch allocation or total process memory.
An explicitly empty enablement variable is invalid; omit it to select the default.

The core CI quality runner requires all four real Kafka custody cases: a killed
consumer process with fsynced raw bytes and a positive next-offset restart marker,
unparked Flush refusal, failed DLQ publication, and successful original-byte DLQ
publication. Its JSON evidence checker rejects missing cases, skips, failures and
an incomplete package, and requires both phase-handoff regressions. The process-crash
fixture retains 45 seconds for setup, actual commit and exclusive Stable membership.
Only still-active successful setup may hand off immediately before kill to a separate
45-second crash-recovery phase covering kill/join, membership expiry, disk custody
and marker publication. Only successful active recovery may hand off to the
five-second positive restart read. The configured context-budget envelope is
45 + 45 + 5 seconds, not a hard whole-process wall-clock guarantee: synchronous
disk I/O, cleanup and scheduling are not preempted by these contexts.
No phase resets its deadline. Other modes keep their original shared
45-second lifecycle deadline. These are disposable
test budgets, not production SLOs or changes to the Kafka client's session timeout.
This is fixture qualification, not production activation.

### Optional finite snapshot owner

The snapshot owner is disabled unless `LAKE_SNAPSHOT_ENABLED=true`. It derives a
finite, tenant-pure inspection object from the **active landing run's currently
open generated files**. It does not enumerate previous process runs, import old
NDJSON, establish offset completeness, or replace the NATS archiver, feature
materializer or financial history. Existing landing files and row output remain
unchanged. Snapshot capture requires raw custody and generated run identity;
explicit legacy append identities are not admitted to this mode.

The gateway remains the identity authority. Its optional lake routes require
explicit `lake.snapshot.create` or `lake.snapshot.read` capability grants and
forward a typed caller over exact-peer gRPC/mTLS. V1 refuses every nonempty
portfolio restriction: a tenant-wide grant cannot stand in for portfolio-scoped
authorization. The lake owner serves only its explicitly configured tenant and
tenant-RLS pool. A different actor cannot reuse another actor's request ID.
Create and all idempotent retries return only the original REQUESTED acceptance;
Get and Query independently require read authority, including for failed objects.

`kanz-lake-snapshot` uses the existing operator gateway authentication/signing
client. It exposes create, get, list, count and failures operations without SQL,
worker addresses, file paths, tenant overrides or caller-identity flags. Selection
uses exact event types and a half-open event-time interval. Returned events contain
only ID, type, exact seconds/nanos, a dense selected-tenant ordinal and closed
decode status. Duplicate order follows the finite cut (sorted owned files, then
line order), not reconstructed cross-file Kafka arrival order; foreign-row positions and raw decode
error text are not exposed. Raw EventFrames and Decimal encodings remain exact
inside the private Parquet object rather than being remarshal-derived history.

Accepted creation and each lifecycle transition enqueue canonical typed FACTs in
the same PostgreSQL transaction. The first cut is persisted once. Retries reopen
only its exact no-follow owned paths and verify device/inode, prefix length and
hash; appended suffixes are excluded, and replacement/truncation refuses. The
session fence and generation CAS govern publication. Output uses unique fixed
generation names, fsync and no-replace rename before READY commits. A stale output
can remain an unregistered orphan; it cannot overwrite an accepted generation.

Admission is bounded to 16 retained requests, including failed requests, with at
most eight pending and three publication generations per request. Each generation
has at most one 96 MiB Parquet member and one 64 KiB manifest, in either its fixed
staging or final directory. This bounds those derived file contents to
4,834,983,936 bytes per configured tenant owner, excluding filesystem metadata,
database/outbox storage, broker storage, the executable and existing landing
retention. Cut metadata resides in PostgreSQL; this path does not create private
landing copies. Exhaustion refuses further admission. No automatic age deletion,
unknown-path cleanup or operator cleanup capability is implemented.

One nonqueueing operation reservation precedes capture, Parquet reads and protobuf
encoding and remains owned through actual child and pipe join. Input traversal is
at most 64 MiB and 100,000 scanned rows, including foreign/unselected rows. Query
reads at most 96 MiB Parquet and emits at most 1 MiB/1,000 rows. The one-call worker
receives no identity credentials, paths, URLs or SQL. Linux launch verifies a
root-owned nonwritable ELF and its configured hash, then executes that same open
descriptor with no arguments and an empty environment. The worker's own sandbox,
read-only deployed image, combined parent/child memory budget and actual process
cleanup require separate Linux qualification; file mode/hash alone do not prove
filesystem immutability. The existing static lake image is not a qualified image
for the GNU worker and must not be enabled unchanged.

Broker setup is explicit and offline. `kanz-tenantgen -tenant <existing-account>
-snapshot-account-candidate` emits a bounded candidate bundle with the preserved
ConfigMap, exact stream profile and explicit `ARCHIVER_LAKE_SNAPSHOTS=true`
setting for the existing same-account tenant archiver. Its only added archiver
permission is the lifecycle subscription; its Kafka dead-letter path is unchanged.
It never reads live configuration, reloads a
broker or provisions a user/stream. Only the reviewed hash-bound embedded account
baseline is parsed. The existing lake SVID may map to exactly one account; all
unrelated account semantics and the default compute set remain unchanged.
The workload can publish lifecycle FACTs and consume Requested but cannot create,
update, purge or delete streams. An administrator must provision the candidate
and the canonical 720-hour DLQ route; the existing archiver must register its
native durable before snapshot startup. Enabled snapshot config uses the canonical
tenant qualifier to add `<tenant>.lake.snapshot` (the legacy `lake.snapshot` only
for `__system__`), matching the existing archiver without changing disabled-mode
landing topics or duplicating an explicitly configured qualified topic.

Startup requires the authenticated account key and workload user from the broker's
user-info service, the exact bounded retained stream profile, canonical parking
custody, and the registered archiver filter/default delivery contract. A registered
consumer is not proof of a live process, successful Kafka archival or backup. Identical
profiles in another account do not establish identity. The profile has no age
expiry, 64 MiB/10,000-message limits and discard-new behavior: full capacity refuses
publication while retaining older work and the durable outbox. Two consumer slots
cover the snapshot owner and existing archiver. The latter retains its native
finite delivery budget; no unlimited archive retry is claimed. Terminal Requested
poison uses the existing DLQ with original bytes/headers. Parking failure, including
oversized original headers plus failure metadata, retains the original delivery
and fails readiness; no bytes are truncated to force parking. Snapshot-only
connections disable reconnect. Disconnect or publication/handler failure makes
readiness fail and cancels owner admission/work, including existing gateway
connections. Restart cannot restore readiness solely by reconnecting: it must
reprove account/profile identity and confirm bounded owned outbox catch-up.
This does not promise instantaneous revocation of already-emitted bytes or
protection against privileged live broker configuration changes.

Actual worker, PostgreSQL and dedicated broker tests are dependency-gated locally.
Their skips are not compatibility, confinement or deployment evidence. Enabling
this optional owner requires the dedicated source/binary-bound Linux proof,
gateway network boundary, scoped broker/PG configuration and qualified resource
budget; the default deployment is not activated by this source implementation.

The disposable process-crash proof uses one 240-second parent observation context
and passes that same absolute deadline to child owners and the archiver fixture.
Its allowance covers two startup ceilings (60 seconds), two work ceilings (60),
native dead-delivery (60), control observations (20), cleanup observations (20)
and scheduling margin (20). It neither resets phases nor changes production
30-second work, five-second cleanup observations or the native 60-second AckWait.
Cleanup reports success only after process and output-pipe joins; an expired
ten-second fixture stop observation fails, then kills and joins the owned child.
These contexts are not a hard operating-system wall-clock guarantee.
The crash witness blocks only the fixture READY transaction after real worker
conversion and immutable file publication, kills the owner, verifies its database
sessions disappeared and READY/Published did not commit, then restarts through
native redelivery. The unchanged persisted cut must exclude both an appended
old-file suffix and a new landing run. The separate existing-archiver subprocess
proof requires exact Kafka bytes/headers, restart continuation and foreign-tenant
parking followed by a positive next marker; neither fixture establishes deployment
identity, Kafka ACLs or disaster-recovery coverage.
