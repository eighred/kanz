# DR-01b — Postgres PITR + cross-region replicas

Disaster recovery for the stateful databases:

| DB | Holds | Cluster |
|---|---|---|
| risk state (PERS-01) | portfolio positions, risk state | `kanz-risk` |
| schema registry (EVT-16) | registered payload schemas | `kanz-registry` |
| book-of-record (PARITY-02) | IBOR ledger journal+snapshots, alternatives fund journal, wealth household book, datamaster golden records + exception queue | `kanz-books` |

(The market-data history and the audit log are append-only/WORM stores with
their own retention; the *transactional* state that DR must restore is these.)

## Coverage of every migration-owning service

Every service under `services/*/migrations/` owns a Postgres schema, and **all of
them are listed here, including the ones that are not covered** — a service
absent from this table is a service whose DR posture nobody has decided, and
that absence is invisible. Thirteen own one today; the count is deliberately not
the invariant, because a number in prose goes stale silently.
`test/arch/dr_postgres_coverage_test.go` derives the list from the filesystem, so
a fourteenth migrations directory cannot appear without a decision and a row
here.

The **DR status** column is machine-checked against that Go classification: the
words `covered`, `excluded` and `NOT COVERED` are load-bearing, and a row that
disagrees with the classification fails the build. Mid-incident this table is
what gets read, so it is not allowed to be the stale copy.

| Service | DR status | Cluster / reason |
|---|---|---|
| `risk-engine` | covered | `kanz-risk` |
| `schema-registry` | covered | `kanz-registry` |
| `accounting` | covered | `kanz-books` (IBOR ledger journal + snapshots) |
| `alternatives` | covered | `kanz-books` (fund journal) |
| `wealth` | covered | `kanz-books` (household book) |
| `datamaster` | covered | `kanz-books` (golden records + exception queue) |
| `market-data` | excluded | append-only history with its own retention; re-ingestable from the feed |
| `audit` | excluded | WORM store with its own tamper-resistant retention (AUDIT-01b) |
| `oms` | **NOT COVERED** | `orders`, `positions`, `position_fills`. See the warning below. |
| `venue-binance` | **NOT COVERED** | `venue_orders` — the exchange-order ↔ kanz-order mapping reconciliation depends on |
| `venue-okx` | **NOT COVERED** | `venue_orders` — same |
| `regulatory` | **NOT COVERED** | `audit_chain_links` — the tamper-evidence chain |
| `tv-sync` | **NOT COVERED** | `tv_facts` — a projection, rebuildable from the event log, so the weakest exposure of the five |

### The five uncovered stores

They hold real transactional state. They are now CLASSIFIED — the rows above say
so, and the guard says so on every run — but classified is not covered: they are
in no cluster and carry no exclusion, so until this is resolved a region failover
starts them empty. The gap is recorded, not closed, and the difference matters
because a table that lists them is not a backup that restores them.

**The OMS is the one that matters most.** After a failover it would come up
against an empty order store, and `SweepInterrupted` would log `count=0` — the
*same line a healthy clean start produces*. There is no signal distinguishing
"no interrupted orders" from "no orders at all, because the store is gone",
so the platform would report normal startup while holding positions at an
exchange that it has no record of.

This is why `CLAUDE.md` sequences **M3 after this issue**: placing real orders
against a store that may not be backed up is the one ordering error with an
unrecoverable failure mode.

Resolving it requires deciding which cluster each belongs to (or that it is
genuinely excludable) and adding the database to that cluster — tracked by
**#60**. Note that database naming across the deploy manifests, the DSN
secrets and this document has been reported as inconsistent, so the mapping
should be settled against the running config rather than any single document
(**#59**).

**Why the five cannot simply be added here.** Each of them reads its DSN from
Vault at `kv/kanz/<service>` (see `infra/security/secrets/secretproviderclass.yaml`);
nothing in this repository says which Postgres host that DSN points at. Placing
a service in `kanz-books` would assert that its data already lives in that
cluster — an assertion only the running config can settle. Recording them as
uncovered is the smaller, true claim; guessing a cluster would produce a table
that reads as coverage while the failover promoted a database the service does
not use.

### Adding a cluster (what "placing" a service actually costs)

A store is not covered because a `Cluster` exists. Three files must agree, and
`test/arch/dr_postgres_coverage_test.go` fails until they do:

| File | What it adds | What its absence costs |
|---|---|---|
| `cluster.yaml` | the primary + `barmanObjectStore` + `archive_timeout` + `retentionPolicy` + a `ScheduledBackup` | WAL with no base backup is not PITR |
| `replica.yaml` | the DR-region standby replaying that WAL | a failover has nothing to promote; recovery becomes a restore-from-scratch inside the RTO |
| `failover.sh` | the cluster name in the `for c in …` promote loop | the standby stays read-only while the services depending on it start anyway |

That last one is the same silent failure this issue is about, reached by a
different route: not a missing backup, but a backup nobody promotes. The
services come up, reach a database, and it is not theirs.

The `kanz-books` cluster hosts one database per service; each service's schema is
defined by its `services/<svc>/migrations/*.sql` (applied in lexical order at
deploy). The stores are event-sourced (ledger, fund) or replace-on-write
projections (book, golden records) — the journal/blob is the source of truth, so
the continuous-WAL + daily-base-backup PITR contract below applies unchanged.

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
