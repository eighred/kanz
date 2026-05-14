# NATS JetStream — live spine

Provisions the NATS JetStream cluster that carries the Kanz event backbone's
**live spine**. Kafka (EVT-09) is the durable log of record; NATS holds a
short-retention live tier for low-latency fan-out.

## Layout

| File | Purpose |
|---|---|
| `namespace.yaml` | `kanz-messaging` namespace |
| `nats.yaml` | 3-node JetStream cluster — ConfigMap, headless + client Services, StatefulSet, PodDisruptionBudget |
| `bootstrap-job.yaml` | ConfigMap + Job that creates the streams/consumers (idempotent) |
| `smoke-test.sh` | pub/sub + persistence smoke test |

## Streams

One stream per domain, binding `{domain}.>` per
`kanz-schemas/docs/subject-taxonomy.md` §4. File storage, 3 replicas, `limits`
retention, `old`-discard, 2m duplicate window (broker-side idempotency keyed on
the envelope `idempotency_key`).

| Stream | Subjects | Max age |
|---|---|---|
| `MARKET` `RISK` `EXECUTION` `INFERENCE` | `<domain>.>` | 24h |
| `PLATFORM` `DATA` | `<domain>.>` | 168h (lifecycle + FACT-grade data-quality) |
| `OBSERVABILITY` | `observability.>` | 1h (ephemeral metrics) |
| `DLQ` | `dlq.>` | 720h (poison-event investigation runway) |

Replay streams (`replay.{run_id}.*`) are created per-run by replay tooling
(EVT-20), not provisioned here.

## Deploy

```sh
kubectl apply -f namespace.yaml -f nats.yaml
kubectl -n kanz-messaging rollout status statefulset/nats   # wait for ready
kubectl apply -f bootstrap-job.yaml
kubectl -n kanz-messaging wait --for=condition=complete job/nats-bootstrap
```

## Smoke test

```sh
kubectl -n kanz-messaging port-forward svc/nats 4222:4222 &
NATS_URL=nats://localhost:4222 ./smoke-test.sh
```

Expects the `nats` CLI locally (or run inside `natsio/nats-box`). Publishes
under `market.smoketest.>`, asserts live delivery + JetStream persistence, then
purges the test subject.
