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
| DR-01d | `failover.sh` + the region-failover runbook below | ordered, idempotent region cutover (promote DBs → rebuild spine → start services → shift traffic) |
| DR-01e | the DR-drill runbook below | quarterly live-failover drill measuring RPO/RTO against the targets |

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

## Runbooks

`failover.sh` is runbook-as-code — it IS the procedure below, executable. The two
pages here are the human wrapper around it: when to declare, what to verify, and
the quarterly drill that proves the objective is real rather than aspirational.

### Region failover (disaster recovery)

Severity **page** · rehearsed by `infra/dr/failover.sh`.


Cut the platform over to the DR region after a primary-region loss. Target
**RPO ≤ 1min, RTO ≤ 15min** (validated quarterly by the DR drill below). The
orchestration is `infra/dr/failover.sh` — this runbook is the human wrapper
around it.

#### When to declare

Primary region is unreachable/unrecoverable within the RTO (control-plane down,
AZ-wide outage that PDBs/multi-AZ can't absorb, data-plane loss). Region failover
is a **human decision** — it is not auto-triggered. Page the incident commander.

#### Preconditions (continuously true, verify at declaration)

- DR-01a: MirrorMaker 2 replicating the Kafka log into DR (`kafka-mm2` healthy;
  replication lag from the heartbeats topic < 1min).
- DR-01b: Postgres warm standbys replaying in DR (`cnpg status` lag < 1min).
- DR cluster reachable as the `DR_CTX` kube-context.

#### Cut over

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

#### Confirm recovered

```sh
kubectl --context <dr-context> -n kanz-services get pods         # services Ready
kubectl --context <dr-context> -n kanz-services logs deploy/risk-engine --tail=20
curl -fsS https://<dr-endpoint>/readyz                           # 200
```

Risk responses should leave RISK-11 degraded mode once the spine is repopulated
and AsOf catches up. Audit + reconciliation continue against the promoted DBs.

#### Failback (after the primary region returns)

Do NOT just flip back — the primary is now stale. Re-establish the primary as a
replica of the (now-authoritative) DR region, let it catch up, then cut back
during a maintenance window using this same procedure with the contexts swapped.
Treat failback as a planned change, not an emergency.

#### Root-cause pointers

Replication health: `infra/dr/kafka` (MM2 heartbeats), `infra/dr/postgres`
(`cnpg status`). Spine rebuild: `infra/dr/nats`. The drill that proves all of
this: the DR drill below.

### DR drill (quarterly)

Severity **ticket** · rehearsed by `infra/dr/failover.sh`.


A scheduled, supervised **live failover** to the DR region that proves the
**RPO ≤ 1min / RTO ≤ 15min** objective is real, not aspirational — the DR analog
of the SRE-01d GameDay. Run it quarterly; a drill that doesn't actually cut over
proves nothing.

#### Cadence & ownership

Quarterly, business hours, incident commander + DB + platform on-call present.
Announce it (it is a real failover). File the results (RPO/RTO measured) as the
audit record that the continuity control is tested.

#### Drive live-shaped load first (PARITY-05e)

A drill against an idle system proves nothing — RPO is only real when writes are
in flight at cutover. Seed a production-shaped book and stream writes throughout
the drill using the PARITY-05d harness, pointed at the PRIMARY region:

```sh
SEED_PORTFOLIOS=2000 SEED_NATS_URL=nats://<primary>:4222 go run ./test/load/seed
RATE=2000 DURATION=20m PORTFOLIOS=2000 INGEST_NATS_URL=nats://<primary>:4222 \
  go run ./test/load/ingest &          # writes flowing across every store during the cut
```

The in-flight writes are what the RPO measurement below actually bounds — the
events published in the last replication interval are the ones a real outage
would risk losing.

#### Before (capture the baseline)

Record the replication lag at T0 — this is the **measured RPO** — across **every
PARITY-02 store** (all three Postgres clusters) and the log:

```sh
# Kafka (DR-01a): now − latest replicated heartbeat timestamp.
kubectl --context <dr> -n kanz-messaging exec kafka-0 -- \
  /opt/kafka/bin/kafka-console-consumer.sh --bootstrap-server kafka:9092 \
  --topic heartbeats --from-beginning --timeout-ms 5000 | tail -1
# Postgres (DR-01b): standby replay lag for each cluster. kanz-risk = risk state;
# kanz-registry = model/lineage registry; kanz-books = IBOR ledger + alternatives
# + wealth + datamaster (every PARITY-02b store). A drill that checks only
# kanz-risk misses the book-of-record — check all three.
for c in kanz-risk kanz-registry kanz-books; do
  echo "== $c =="; kubectl --context <dr> -n kanz-data cnpg status "$c" | grep -i lag
done
```

**Gate:** every lag < 1min, or the drill fails on RPO before you cut over.

> **market-data (price history)** has no dedicated DR cluster by design: it is a
> projection whose source of truth is the durable Kafka log, so it is
> *reconstructed* from the rebuilt spine (step 3 below) rather than PITR-restored —
> the same reconstructable-from-log stance as the NATS spine. Nothing to promote;
> verify it re-derives (synthetic price query below).

#### Run the failover (measure RTO)

Start a timer, then execute the DR-01d procedure:

```sh
time ( DR_CTX=<dr> ./infra/dr/failover.sh )   # wall-clock to services-Ready = RTO
```

Then flip a *test* DNS/LB weight (or a drill hostname) to DR and run a synthetic
transaction **against every PARITY-02 store** — a store that promoted but does not
serve is a silent DR failure:

```sh
GW=https://<dr-drill-host>
curl -fsS "$GW/v1/portfolios/PF1-0001/exposure"        >/dev/null  # risk state  (kanz-risk)
curl -fsS "$GW/v1/households/HH-0001"                   >/dev/null  # wealth book (kanz-books)
curl -fsS "$GW/v1/securities/AAPL"                      >/dev/null  # datamaster  (kanz-books)
curl -fsS "$GW/v1/prices/AAPL"                          >/dev/null  # market-data (reconstructed)
curl -fsS "$GW/v1/nav/PF1-0001?as_of=$(date -u +%FT%TZ)" >/dev/null # IBOR ledger (kanz-books)
```

**Gate:** wall-clock from declaration to all five synthetic reads succeeding
≤ 15min. Then confirm the in-flight write stream (still running against the
promoted primary via the DR spine) resumes — a durable write acked in DR proves
the book-of-record is writable, not just readable.

#### Success criteria

| Objective | Measure | Target |
|---|---|---|
| RPO | max replication lag at T0 (Kafka + all three Postgres clusters) | ≤ 1min |
| RTO | declaration → all synthetic reads succeed | ≤ 15min |
| Store coverage | every PARITY-02 store promoted + serving (5 synthetic txns) | 5/5 |
| Writability | a durable write acked post-cutover (ingest stream resumes in DR) | acked |
| Integrity | post-failover audit chain verify (`/v1/audit/verify`) | 200 / verified |
| Data | no sequence gap (`kanz_data_gap_missing_total` flat) across cutover | 0 |

#### After (fail back + record)

1. Fail back per the region-failover runbook above (re-replicate primary from DR, cut back in a window).
2. File the measured RPO/RTO + any gate miss as a finding; a missed gate is a DR
   regression — fix it (capacity, archive cadence, rebuild parallelism) before
   the next quarter, same discipline as a GameDay verify abort (SRE-01d).
