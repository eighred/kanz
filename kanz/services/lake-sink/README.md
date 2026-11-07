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
