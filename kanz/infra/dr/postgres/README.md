# DR-01b — Postgres PITR + cross-region replicas

Disaster recovery for the stateful databases:

| DB | Holds | Cluster |
|---|---|---|
| risk state (PERS-01) | portfolio positions, risk state | `kanz-risk` |
| schema registry (EVT-16) | registered payload schemas | `kanz-registry` |
| book-of-record (PARITY-02) | IBOR ledger journal+snapshots, alternatives fund journal, wealth household book, datamaster golden records + exception queue, tv-sync fact log | `kanz-books` |
| order path (OMS + venues) | orders, positions, position_fills, and both adapters' venue_orders | `kanz-orders` |
| compliance evidence (REG-02) | audit_chain_links — the filing hash chain | `kanz-compliance` |
| platform identity (#364) | identity_users, identity_invites — the platform's own accounts and their Argon2id credentials | `kanz-identity` |

(The market-data history and the audit log are append-only/WORM stores with
their own retention; the *transactional* state that DR must restore is these.
Read the `market-data` row in the posture table before extending that sentence
to everything in that database — its bitemporal tables are not replayable.)

**A cluster boundary here is a restore-timeline boundary.** PITR rewinds every
database in a cluster together, so the grouping answers one question: *what must
restore to the same instant, and what must be able to restore independently?*
The order path shares a cluster because an order and the venue mapping that ties
it to the exchange must come back together. The compliance chain is alone
because it is evidence *about* the other stores and must not be rewound with
them. Nothing here is grouped for tidiness.

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
| `identity` | covered | `kanz-identity` — its **own** cluster (`identity_users`, `identity_invites`, #364). Credentials exist nowhere else: a failover onto an empty store locks **everyone** out, including whoever would run the recovery, so this is the one standby whose absence makes every other standby unreachable. Own cluster because PITR is per-cluster — co-locating with `kanz-books` would tie a password rotation to the ledger's restore timeline, silently re-enabling disabled accounts on any rewind. |
| `market-data` | excluded | `price_observations` is append-only history with its own retention, re-ingestable from the feed. `contract_terms` (#345) and `ohlcv_bars` (#425) are **not**: a venue serves its *current* view of a specification or a candle, so a refeed restores today's values under today's `knowledge_time` and the original observation plus every restatement between are gone. What a restore loses is not the prices — it is the evidence of *what was knowable when*. `ingestion_coverage` (#591) is not replayable **at all** — nothing can reconstruct whether a feed was live last July — but losing it **fails safe**: an absent row reads as UNKNOWN and refuses a claim, where a refed candle supports a wrong one. The exclusion holds only while no sizing decision reaching real capital is derived from this store. |
| `audit` | excluded | WORM store with its own tamper-resistant retention (AUDIT-01b) |
| `oms` | covered | `kanz-orders` (`orders`, `positions`, `position_fills`). **Read "What `covered` does not mean" below before relying on this row.** |
| `venue-binance` | covered | `kanz-orders` (`venue_orders`) — same cluster as the OMS *on purpose*: one restore timeline for the order path |
| `venue-okx` | covered | `kanz-orders` (`venue_orders`) — same |
| `regulatory` | covered | `kanz-compliance` — its own cluster (`audit_chain_links`), deliberately not co-located with anything it attests to |
| `tv-sync` | covered | `kanz-books` (`tv_facts`) — covered rather than excluded; the rebuild story does not survive the config (see below) |

### ⚠ What `covered` does not mean — read this before acting on ANY row above

**`covered` means the DR wiring is DECLARED, not that the running service is
bound to it.** Every service reads its DSN from Vault at `kv/kanz/<service>` (via
its `<service>-db` SecretProviderClass → `<SERVICE>_DATABASE_URL_FILE`), and **no
file in this repository sets that value.** The guard checks three manifests
agree; it cannot see which database a pod actually opens.

So for every service placed by **#60**, the cluster could be faithfully backing
up and promoting an **empty** database while the real store sits on a host with
no PITR — which is #60's own failure mode wearing a green check. Each Vault entry
must name its cluster's read-write service (CNPG convention:
`<cluster>-rw.<namespace>.svc`):

| Vault path | must resolve to |
|---|---|
| `kv/kanz/oms` | `kanz-orders-rw.kanz-data.svc` |
| `kv/kanz/venue-binance` | `kanz-orders-rw.kanz-data.svc` |
| `kv/kanz/venue-okx` | `kanz-orders-rw.kanz-data.svc` |
| `kv/kanz/regulatory` | `kanz-compliance-rw.kanz-data.svc` |
| `kv/kanz/tv-sync` | `kanz-books-rw.kanz-data.svc` |

Until **#59** confirms these, each service may still be writing to whatever host
its DSN names today. Verify the live DSN before you trust a `covered` row
mid-incident (password-redacted):

```sh
kubectl -n kanz-services exec deploy/oms -- sh -c 'cat "$OMS_DATABASE_URL_FILE"' | sed 's#://[^@]*@#://***@#'
```

Three of these five were given **NEW, empty clusters** (`kanz-orders`,
`kanz-compliance`), so the row asserts nothing about where their data lives
today — which is precisely why the repoint is still outstanding. The two placed
into existing clusters (`venue-*` into `kanz-orders`, `tv-sync` into
`kanz-books`) additionally assert that a database for them exists *in* that
cluster; if it does not, creating it is part of the same repoint.

`oms` is the row where being wrong is unrecoverable, but the caveat is not an
`oms` caveat — it applies to every `covered` row in the table, including the six
that predate #60.

### Why each store sits where it does

**`oms` → `kanz-orders`, its own cluster and not a database in `kanz-books`.**
PITR is per-cluster. Restoring the order book to an instant before a bad sweep
rewinds *every* database in that cluster to the same instant — so sharing
`kanz-books` would make a correction on the trading path silently roll back the
IBOR ledger, the fund journal, the household book and the golden records.
Isolated, the blast radius of an order-store restore is the order store.

It was placed first because its absence is the *silent* one: after a failover
against an empty order store, `SweepInterrupted` logs `count=0` — the *same line
a healthy clean start produces*. Nothing distinguishes "no interrupted orders"
from "no orders at all", so the platform reports normal startup while holding
positions at an exchange it has no record of. That is why `CLAUDE.md` sequences
**M3 after this issue**.

**`venue-binance` + `venue-okx` → `kanz-orders`, beside the OMS.** The same
per-cluster PITR property that isolates the order store is the reason these join
it rather than getting clusters of their own. `venue_orders` is not an
independent store: the OMS admits an order at its own primary key and only *then*
calls the adapter, which writes the row tying the exchange order back to it — one
logical transaction on the order path. Separate clusters would mean **separate
restore timelines**: restore `orders` to T while `venue_orders` sits at T′ and
you get orphan venue mappings, or — worse — orders with no mapping back to the
exchange order at all. That mapping is exactly what the idempotency and recovery
path reads: the user-data websocket delivers an execution report carrying only a
`clOrdId`, and without the row there is nothing to enrich it from and nothing to
tell the reconciler what should be open. **One cluster is one timeline, so the
order path restores coherently or not at all.**

**`regulatory` → `kanz-compliance`, alone.** The only placement here argued from
*independence* rather than coherence. `audit_chain_links` is evidence **about**
the trading path and the books — each row is a filing's signature folded into its
predecessor plus the exact canonical bytes that were signed. Because PITR is
per-cluster, co-locating it would mean any point-in-time restore of the thing
being attested to *also rewinds the attestation*. Evidence that gets rewound
whenever the thing it attests to gets rewound is not evidence: the chain could
not then distinguish "those filings never happened" from "the record of them was
rolled back", which is the one discrimination a tamper-evidence structure exists
to make. Independent, the chain outlives the restore and the gap is visible in
it. It is also not excludable at any price — losing it does not corrupt the
chain, it makes the chain unverifiable, and the canonical signed bytes exist
nowhere else to re-derive them from.

**`tv-sync` → `kanz-books`, covered rather than excluded.** Its own comments call
`tv_facts` "rebuildable from the event log". Chased to the configuration, that
does not hold after a region loss — and it is the kind of claim that only fails
when it is needed:

| Where | Setting | What it means |
|---|---|---|
| `infra/nats/bootstrap-job.yaml` | `EXECUTION` stream (`execution.>,strategy.>,order.>`) `max_age` **24h** | the live spine holds at most a day of the FACTs tv-sync folds |
| `infra/kafka/topics-job.yaml` | `order.order` → `delete`, `retention.ms=2592000000` | Kafka, the archival path, *does* keep 30 days |
| `infra/dr/nats/rebuild-job.yaml` | `NATS_REBUILD_SINCE=24h`, and `order.order` is **not** in `NATS_REBUILD_STATE_TOPICS` | the DR rebuild reads a 24h window of that 30-day log, not offset 0 |

So the 30-day Kafka log is real, and the rebuild does not use it. Two further
facts settle it: the rebuild's only sink is the **bus** — it restores the spine,
and nothing anywhere re-drives this projection (the sole writer of `tv_facts` is
the running service, whose `Rehydrate` reads `tv_facts` itself, which is circular
when the table is empty) — and the rebuild job drains only the un-prefixed
`__system__` topics, so for any onboarded tenant it replays nothing and **exits
0**. No fill is persisted anywhere else, so what would be lost is not a cache of
something durable.

It is in `kanz-books` rather than `kanz-orders` because it is the **read** side:
nothing on the order path reads `tv_facts`, so it has no transactional coupling
to `orders` and must not share the money path's restore timeline in either
direction. `kanz-books` already holds exactly this class — projections whose
source of truth is the append-only journal.

The general rule this settles: **covering something unnecessarily is cheap;
excluding it on a rebuild story nobody has exercised is how #60 happened.** The
two standing exclusions (`market-data`, `audit`) are not of that shape — each has
an independently documented mechanism (`docs/runbooks/dr-drill.md` for
market-data; object-lock/WORM for audit), not an inference.

Database naming across the deploy manifests, the DSN secrets and this document
has been reported as inconsistent, so every mapping above should still be settled
against the running config rather than any single document (**#59**) — see the
Vault table in the call-out above.

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

`failover.sh` step 4's `rollout status` wait list is a **fourth** place, and it
is *not* machine-checked: a covered service missing from it is never waited on
during promotion, and because the loop tolerates a missing deploy with `|| true`,
an absent name and a present-but-failing name print exactly the same thing —
nothing. Add every covered service there too.

`kanz-books` and `kanz-orders` each host one database per service; each service's
schema is defined by its `services/<svc>/migrations/*.sql` (applied in lexical
order at deploy). `kanz-books`'s stores are event-sourced (ledger, fund) or
replace-on-write projections (book, golden records, `tv_facts`) — the
journal/blob is the source of truth, so the continuous-WAL + daily-base-backup
PITR contract below applies unchanged.

Base-backup slots are 30 minutes apart so they do not contend for the same
object-store bandwidth: `kanz-risk` 02:00, `kanz-registry` 02:30, `kanz-books`
03:00, `kanz-orders` 03:30, `kanz-compliance` 04:00. The last is also the right
*order*: the evidence chain is backed up after the stores it attests to, so its
snapshot covers at least everything already captured in theirs.

### Known gap in this procedure (not introduced by the coverage work)

`failover.sh` step 4 runs `kubectl scale deploy --all --replicas=2`.
`infra/deploy/tv-sync-deploy.yaml` pins `replicas: 1` and states it is a
**correctness** bound, not a capacity one — two pods split the fact stream, so
each folds only part of it and every pod's book is wrong. The blanket scale
overrides that pin on every failover, so a DR cutover as written brings `tv-sync`
up in the one configuration its own manifest forbids. Backing up `tv_facts` does
not fix that; it is a separate defect in the failover procedure, recorded here
and in a comment at the wait list rather than repaired in passing, because
narrowing `--all` changes every service's failover behaviour.

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
