# Architecture reference

## Event and identity model

State changes flow as typed FACTs through NATS to domain services and Postgres.
The gateway is the sole external identity authority and injects Kanz principal
headers. That trust is valid only where NetworkPolicy makes the gateway the
upstream's sole reachable caller.

The execution path is:

```text
strategy -> intent -> risk -> compliance -> mandate -> OMS
         -> execution router -> venue adapter -> exchange
```

No admin, MCP or test helper may skip a hop. OMS to adapter remains typed
`venue.v1` gRPC/mTLS; exchange protocols and credentials remain inside adapters.

## Coordination at N replicas

Idempotent handlers are the correctness floor. Per-process deduplication is an
optimization. Shared coordination is opt-in only where partition ownership does
not already serialize work.

- `DedupWindow` is per-instance; `RedisDedup` coordinates a shared seen-set.
- Shared dedup backend failure opens to handler idempotency, never fabricates a
  hit that blocks consumption.
- Partition-keyed gap, watermark, staleness and drift detectors remain local
  because the consumer group already grants single partition ownership.
- `platform.model` is the model registry's source-of-truth append log; replicas
  replay from offset zero and treat their registry as a materialized cache.
- Kafka is derived from NATS through the single archiver. Do not recreate a
  NATS/Kafka reconciler.

## Interfaces

MCP is a read plane over authorized domain APIs. It cannot place, cancel or
amend orders or call a venue. The TUI may be retired only after every live human
operation has a verified Web equivalent; unused-by-Web is not dead-code proof.
