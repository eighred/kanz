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
| `topics-job.yaml` | ConfigMap + Job that provisions the `__system__` (un-prefixed) topics/retention/compaction (idempotent) |
| `tenancy.yaml` | MT-01c: per-tenant prefixed topics + PREFIXED ACLs (templated by TENANT + PRINCIPAL) |
| `smoke-test.sh` | produce/consume smoke test |

## Topics

`topic = {domain}.{entity}` per `kanz-schemas/docs/subject-taxonomy.md` §5;
`event_type` is discriminated within the topic. Every delete-policy event topic
has a paired `dlq.{name}` for poison events. Auto-create is disabled — topics
exist only if provisioned here. RF 3, `min.insync.replicas` 2.

**The topic list is not repeated here. It lives in the `<<'EOF'` table in
[`topics-job.yaml`](topics-job.yaml), and [`tenancy.yaml`](tenancy.yaml) carries
the same table under a tenant prefix.** This section used to hold a third copy,
and all three drifted independently: the README still listed `market.equity`,
`execution.order`, `data.market_stream`, `platform.model`, `platform.config` and
`observability.model` — six topics no service publishes and two that exist in
neither Job — long after `topics-job.yaml` had been corrected to the real money
path. A table a reader trusts instead of opening the manifest is worse than no
table. `TestTenantTopicTableMatchesTheSystemTable` now holds the two real ones
together, column by column; nothing can hold a prose copy to them.

`topic.For` (`services/archiver/internal/topic/topic.go:56-59`) routes
STATE_SNAPSHOT events to a compacted `{domain}.{entity}.snapshot` sibling.
**No such topic is provisioned in either Job, deliberately** — nothing in
production publishes a STATE_SNAPSHOT (only test fixtures and `test/load/seed`),
and provisioning ahead of a publisher is what produced the stale table above. A
premature publisher therefore fails closed and NACK-loops loudly, which is the
archiver's never-silent rule. Design the publisher and the topic together.

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

## Tenant isolation (MT-01c)

Kafka has no account concept, so tenants are isolated by **topic prefix**:
`{tenant}.{domain}.{entity}`, confined by a PREFIXED ACL on `{tenant}.`. The
KRaft `StandardAuthorizer` runs **deny-by-default**
(`allow.everyone.if.no.acl.found=false`, matching AUTH-01b), so a principal with
no grant gets nothing — tenant A can never touch tenant B's or `__system__`'s
topics. The reserved `__system__` tenant keeps the **un-prefixed** legacy topics
(`topics-job.yaml`) for platform/observability + pre-tenancy events, so existing
logs and consumers are untouched; only new tenants get prefixed topics. The bus
prepends `{tenant}.` from the envelope `tenant_id` (empty/`__system__` → no
prefix) — the bus-wiring follow-up.

**Add a tenant**: copy the `kafka-tenant-acme` Job in `tenancy.yaml`, set
`TENANT` + `PRINCIPAL` (the tenant workload's SVID), and apply. It creates the
prefixed topics + ACLs over the SSL listener as the `kafka-provisioner`
super-user SVID.

**Broker dependency**: SVID certs have an empty subject DN (identity is the URI
SAN), so the broker needs a SPIFFE-aware `principal.builder.class` to surface
`User:spiffe://…` as the ACL principal — `KAFKA_PRINCIPAL_BUILDER_CLASS` is set
to `io.kanz.kafka.SpiffePrincipalBuilder`; shipping that plugin is the SEC-01
companion. The PLAINTEXT:9092 principal (`ANONYMOUS` — inter-broker + in-cluster
bootstrap) stays a super-user until inter-broker SSL lands and 9092 is cut.

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
