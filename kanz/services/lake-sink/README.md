# lake-sink (LAKE-01a)

Schema-evolution-aware CDC sink: streams the durable Kafka log (EVT-09, the
system of record) and lands every event into the lakehouse, decoding each
payload against the EVT-16 schema registry so the table columns track the
*published* schema rather than a compiled-in type.

Because the decode is dynamic (registry descriptor → `dynamicpb` → JSON), a field
added additively to a payload schema surfaces as a new column with **no redeploy**
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
`event_time`. Each process writes its own `part-{instance}` file so concurrent
replicas never interleave. This NDJSON staging is the layer a lakehouse catalog
ingest (Iceberg/Delta via Trino/Spark) commits as a table — concrete table-format
commits (manifests, snapshot isolation) plug in behind the `Sink` interface, so
the binary carries no heavy table-format dependency.

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

## Tests

`go test ./services/lake-sink/...` — decode round-trip over a real
FileDescriptorSet (`common.v1.Decimal`), transient-vs-permanent resolve
classification, immutable-ref caching, Hive partition layout + path-segment
sanitization, and the three CDC decode outcomes (land / ack-envelope-only / NAK).
