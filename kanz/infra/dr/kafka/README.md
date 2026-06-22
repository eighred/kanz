# DR-01a — Kafka cross-region replication (MirrorMaker 2)

Replicates the **log of record** (the `kanz-messaging` Kafka cluster — the
durable backbone everything else is rebuilt from) into a DR region, so a primary
region loss costs at most the replication lag. This is the foundation the rest of
the DR epic stands on: DR-01c rebuilds NATS from this replicated log, DR-01d
fails consumers over to it.

## Topology

**Active/passive, primary → dr only.** MirrorMaker 2 runs **in the DR region**
and *pulls* from primary (`connect-mirror-maker.sh … --clusters dr`): the
replicator must survive a primary-region outage, and a remote consumer riding a
flaky cross-region link is exactly MM2's design point.

**IdentityReplicationPolicy** keeps topic names *identical* across regions —
`risk.portfolio` stays `risk.portfolio` in DR, not `primary.risk.portfolio`. So
on failover (DR-01d) producers/consumers just repoint at the DR bootstrap, and
DR-01c reads the same topic names it would in primary. Safe here because
replication is one-directional (no active/active loop).

| File | Purpose |
|---|---|
| `mirrormaker2.yaml` | MM2 config (ConfigMap) + spiffe-helper + 2-worker Deployment, for the DR cluster's `kanz-messaging` namespace |

## What gets replicated

Every domain topic, snapshot, and `dlq.*` (the DLQ is part of the record) —
`topics = .*` minus MM2/Connect internals. Topic configs are kept in sync
(`sync.topic.configs.enabled`). Consumer-group offsets are translated to DR via
checkpoints + `sync.group.offsets`, so a failed-over consumer resumes near where
it left off rather than from the beginning or the end.

## RPO

Heartbeats/checkpoints/offset-sync run on a 5s interval and `min.insync.replicas`
is 3 on DR's internal topics, so DR trails primary by seconds under healthy
link — inside the **RPO ≤ 1min** target (validated live by DR-01e). Measure
actual lag from the `heartbeats` topic (MirrorHeartbeatConnector stamps a
source timestamp; DR-now minus that timestamp is the replication lag) and the
`MirrorSourceConnector` `replication-latency-ms` metric.

## Before you apply

1. **Primary bootstrap.** Set `primary.bootstrap.servers` in the ConfigMap to
   the primary cluster's cross-region-reachable mTLS endpoint (VPC peering /
   private link / internal LB on `:9094`). `dr.bootstrap.servers` is local.
2. **MM2's ACLs (MT-01c).** Both clusters are deny-by-default with a SPIFFE
   principal builder. MM2 authenticates with its own SVID
   (`spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-mm2`); grant it **READ on
   all topics + groups + the offset-syncs topic on PRIMARY** and **admin
   (create/write) on DR**, or add it to each cluster's `KAFKA_SUPER_USERS`. A
   replicator with no ACLs silently mirrors nothing.
3. **DR cluster exists.** Stand up the peer Kafka cluster (`infra/kafka`) in the
   DR region first; MM2 creates the mirrored topics there on first run.

## Apply

```sh
# In the DR cluster:
kubectl apply -f mirrormaker2.yaml
kubectl -n kanz-messaging logs deploy/kafka-mm2 -c mirrormaker --tail=50
# Confirm topics appearing in DR:
kubectl -n kanz-messaging exec kafka-0 -- \
  /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:9092 --list
```
