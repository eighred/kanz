---
title: Region failover (disaster recovery)
severity: page
gameday: infra/dr/failover.sh
---

# Region failover (disaster recovery)

Cut the platform over to the DR region after a primary-region loss. Target
**RPO ≤ 1min, RTO ≤ 15min** (validated quarterly by `dr-drill.md`). The
orchestration is `infra/dr/failover.sh` — this runbook is the human wrapper
around it.

## When to declare

Primary region is unreachable/unrecoverable within the RTO (control-plane down,
AZ-wide outage that PDBs/multi-AZ can't absorb, data-plane loss). Region failover
is a **human decision** — it is not auto-triggered. Page the incident commander.

## Preconditions (continuously true, verify at declaration)

- DR-01a: MirrorMaker 2 replicating the Kafka log into DR (`kafka-mm2` healthy;
  replication lag from the heartbeats topic < 1min).
- DR-01b: Postgres warm standbys replaying in DR (`cnpg status` lag < 1min).
- DR cluster reachable as the `DR_CTX` kube-context.

## Cut over

Run the orchestration (it is ordered + idempotent; resume a single step with
`STEP=`):

```sh
DR_CTX=<dr-context> ./infra/dr/failover.sh
```

The steps, in required order:

1. **Promote Postgres** (DR-01b) — `cnpg promote` the risk + registry standbys to
   writable primaries.
2. **Messaging** — confirm DR Kafka is present (DR-01a), bring up NATS + provision
   streams.
3. **Rebuild the NATS spine** (DR-01c) — `nats-rebuild` Job drains the recent
   Kafka window onto the live subjects.
4. **Start services** — scale up; they read the promoted DSN from their CSI
   secret (the DR overlay points it at the promoted cluster) and consume the
   rebuilt spine.
5. **Shift traffic** — flip the DNS / global-LB weight to DR (provider-specific;
   the script gates this on api-gateway readiness but the DNS flip is manual).

## Confirm recovered

```sh
kubectl --context <dr-context> -n kanz-services get pods         # services Ready
kubectl --context <dr-context> -n kanz-services logs deploy/risk-engine --tail=20
curl -fsS https://<dr-endpoint>/readyz                           # 200
```

Risk responses should leave RISK-11 degraded mode once the spine is repopulated
and AsOf catches up. Audit + reconciliation continue against the promoted DBs.

## Failback (after the primary region returns)

Do NOT just flip back — the primary is now stale. Re-establish the primary as a
replica of the (now-authoritative) DR region, let it catch up, then cut back
during a maintenance window using this same procedure with the contexts swapped.
Treat failback as a planned change, not an emergency.

## Root-cause pointers

Replication health: `infra/dr/kafka` (MM2 heartbeats), `infra/dr/postgres`
(`cnpg status`). Spine rebuild: `infra/dr/nats`. The drill that proves all of
this: `dr-drill.md`.
