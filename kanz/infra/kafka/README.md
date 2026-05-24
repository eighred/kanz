# Kafka — durable log of record

Provisions the Kafka cluster that is the Kanz event backbone's **durable log of
record**. NATS JetStream (EVT-08) is the live spine; Kafka holds the long-lived,
replayable log that replay tooling (EVT-20) and cold-starting consumers read.

Runs in the `kanz-messaging` namespace — created by the NATS provisioning, or
`kubectl create namespace kanz-messaging`.

## Layout

| File | Purpose |
|---|---|
| `kafka.yaml` | 3-node KRaft cluster (combined broker+controller) — headless + client Services, StatefulSet, PodDisruptionBudget, SEC-01c mTLS listener + spiffe-helper |
| `topics-job.yaml` | ConfigMap + Job that provisions topics/retention/compaction (idempotent) |
| `smoke-test.sh` | produce/consume smoke test |

## Topics

`topic = {domain}.{entity}` per `kanz-schemas/docs/subject-taxonomy.md` §5;
`event_type` is discriminated within the topic. STATE_SNAPSHOT events use a
separate compacted `{domain}.{entity}.snapshot` topic. Every delete-policy event
topic has a paired `dlq.{name}` for poison events. Auto-create is disabled —
topics exist only if provisioned here. RF 3, `min.insync.replicas` 2.

| Topic | Parts | Cleanup | Retention |
|---|---|---|---|
| `market.equity` | 12 | delete | 7d |
| `market.option` | 6 | delete | 7d |
| `risk.portfolio` | 6 | delete | 30d |
| `risk.portfolio.snapshot` | 6 | compact | — |
| `execution.order` | 6 | delete | 30d |
| `execution.order.snapshot` | 6 | compact | — |
| `inference.feature` | 6 | delete | 7d |
| `inference.prediction` | 6 | delete | 30d |
| `platform.model` | 3 | delete | infinite (lifecycle audit) |
| `platform.config` | 3 | compact | — |
| `data.market_stream` | 3 | delete | 30d |
| `data.feature` | 3 | delete | 30d |
| `observability.model` | 3 | delete | 7d |
| `dlq.<name>` | 3 | delete | 30d |

## mTLS (SEC-01c)

The broker presents its SPIRE SVID and requires a client SVID on the **SSL
listener (`:9094`)** — `ssl.client.auth=required`, so a plaintext or untrusted
client is rejected. A `spiffe-helper` sidecar fetches the broker's SVID from the
SEC-01a agent socket (SPIFFE CSI volume), writes PEM files to a memory-backed
`/etc/kafka-certs`, and concatenates cert+key into the combined `keystore.pem`
Kafka's PEM keystore requires. Endpoint identification is disabled (identity is
the SPIFFE URI SAN, not DNS). Go clients dial `:9094` with
`bus.KafkaConfig.TLSConfig` from `transport.ClientTLSConfig` (SEC-01b).

`PLAINTEXT:9092` stays for inter-broker traffic and the in-cluster bootstrap
job during migration; inter-broker SSL + cutting 9092 is a follow-up. **Cert
rotation**: the sidecar rebuilds `keystore.pem` on renewal, but the broker
re-reads it on restart or a `kafka-configs` dynamic update — not yet automatic.

Requires SPIRE (SEC-01a) deployed and `kanz-messaging` SPIFFE-enabled.

## Deploy

```sh
kubectl apply -f kafka.yaml
kubectl -n kanz-messaging rollout status statefulset/kafka   # wait for ready
kubectl apply -f topics-job.yaml
kubectl -n kanz-messaging wait --for=condition=complete job/kafka-topics
```

## Smoke test

Run in-cluster (advertised listeners are in-cluster DNS, so a port-forward
cannot complete a produce/consume round-trip):

```sh
kubectl -n kanz-messaging run kafka-smoke --rm -i --restart=Never \
  --image=apache/kafka:3.9.0 --command -- bash -c "$(cat smoke-test.sh)"
```

Creates a throwaway `smoke-test.<ts>` topic, round-trips a tagged message, and
deletes the topic on exit.
