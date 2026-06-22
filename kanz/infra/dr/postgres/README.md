# DR-01b — Postgres PITR + cross-region replicas

Disaster recovery for the two stateful databases:

| DB | Holds | Cluster |
|---|---|---|
| risk state (PERS-01) | portfolio positions, risk state | `kanz-risk` |
| schema registry (EVT-16) | registered payload schemas | `kanz-registry` |

(The market-data history and the audit log are append-only/WORM stores with
their own retention; the *transactional* state that DR must restore is these two.)

| File | Where | Purpose |
|---|---|---|
| `cluster.yaml` | primary region | primary clusters + continuous WAL archiving + daily base backups |
| `replica.yaml` | DR region | warm-standby replica clusters replaying the archived WAL |

## Why CloudNativePG

A K8s-native operator with built-in barman WAL archiving, PITR, and
replica-cluster DR — no bespoke backup cron, and it reuses the SEC-01d CSI
secret pattern. A managed cloud Postgres (RDS / Cloud SQL) with cross-region read
replicas + PITR is the drop-in equivalent if not self-hosting; the RPO and
restore semantics below are identical.

## RPO

- **Continuous WAL archiving** to the cross-region object store with
  `archive_timeout=30s` bounds the archived-WAL RPO to ≤30s even on an idle DB.
- The **replica cluster** replays that WAL continuously, so the DR copy trails
  the primary by the archive cadence — inside the **RPO ≤ 1min** target
  (DR-01e). For near-zero RPO, add a streaming `connectionParameters` source to
  the replica's `externalClusters` so it also streams when the primary is up.

## PITR (point-in-time recovery)

Daily base backup + continuous WAL ⇒ restore to **any instant** in the 30-day
retention window — the defense against logical corruption a streaming replica
alone can't give (a bad DELETE replicates too). Restore into a fresh cluster:

```yaml
spec:
  bootstrap:
    recovery:
      source: kanz-risk-origin
      recoveryTarget:
        targetTime: "2026-06-22 14:30:00+00"   # the instant to restore to
```

## Failover (DR-01d)

In the DR region, promote the warm standby to a standalone primary, then repoint
the services' DSNs at it:

```sh
kubectl cnpg promote kanz-risk -n kanz-data         # or set spec.replica.enabled=false
kubectl cnpg status   kanz-risk -n kanz-data
```

Services read the DSN from the SEC-01d CSI mount (`*_DATABASE_URL_FILE`); the
DR overlay points that secret at the promoted cluster's service.

## Before you apply

1. Create the `kanz-dr-s3` secret (bucket creds) in `kanz-data` in both regions.
2. Install the CloudNativePG operator once per cluster.
3. Object-lock the backup bucket (WORM) so backups can't be deleted within
   retention — the same tamper-resistance posture as the AUDIT-01b log.
