# Disaster Recovery (DR-01)

Provable cross-region RPO/RTO for the durable log, state stores, and registry —
**RPO ≤ 1min, RTO ≤ 15min**, validated quarterly by a live drill.

The design leans on the architecture's existing split: Kafka is the durable log
of record, NATS is a rebuildable live tier, and only two databases hold
transactional state. So DR replicates the durable things and *rebuilds* the
derivable ones.

| Task | Path | What |
|---|---|---|
| DR-01a | `kafka/` | MirrorMaker 2 replicates the Kafka log of record into the DR region (active/passive, identical topic names) |
| DR-01b | `postgres/` | CloudNativePG primaries with continuous WAL archiving + PITR, and DR-region warm-standby replicas |
| DR-01c | `nats/` | rebuild the live NATS spine from the DR Kafka log (`kanz/tools/natsrebuild`) |
| DR-01d | `failover.sh` + `docs/runbooks/dr.md` | ordered, idempotent region cutover (promote DBs → rebuild spine → start services → shift traffic) |
| DR-01e | `docs/runbooks/dr-drill.md` | quarterly live-failover drill measuring RPO/RTO against the targets |

## Cutover order (why)

Durable state first (Postgres), then the log/spine (Kafka is already replicated;
NATS is rebuilt), then stateless services, then traffic. State before compute
before traffic — a service that comes up before its DB is promoted or its spine
is rebuilt just errors. `failover.sh` enforces this order.

## Standing prerequisites

- DR region cluster with the SPIRE trust domain extended (SEC-01a) so SVIDs are
  issuable there.
- The `kanz-dr-s3` backup-bucket secret in both regions; object-lock the bucket.
- The CloudNativePG operator + Chaos/Argo as used elsewhere installed in DR.
- MirrorMaker 2 (DR-01a) and the Postgres replicas (DR-01b) running continuously
  — DR is only as good as the replication that precedes the incident.
