---
title: DR drill (quarterly)
severity: ticket
gameday: infra/dr/failover.sh
---

# DR drill (quarterly)

A scheduled, supervised **live failover** to the DR region that proves the
**RPO ≤ 1min / RTO ≤ 15min** objective is real, not aspirational — the DR analog
of the SRE-01d GameDay. Run it quarterly; a drill that doesn't actually cut over
proves nothing.

## Cadence & ownership

Quarterly, business hours, incident commander + DB + platform on-call present.
Announce it (it is a real failover). File the results (RPO/RTO measured) as the
audit record that the continuity control is tested.

## Drive live-shaped load first (PARITY-05e)

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

## Before (capture the baseline)

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

## Run the failover (measure RTO)

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

## Success criteria

| Objective | Measure | Target |
|---|---|---|
| RPO | max replication lag at T0 (Kafka + all three Postgres clusters) | ≤ 1min |
| RTO | declaration → all synthetic reads succeed | ≤ 15min |
| Store coverage | every PARITY-02 store promoted + serving (5 synthetic txns) | 5/5 |
| Writability | a durable write acked post-cutover (ingest stream resumes in DR) | acked |
| Integrity | post-failover audit chain verify (`/v1/audit/verify`) | 200 / verified |
| Data | no sequence gap (`kanz_data_gap_missing_total` flat) across cutover | 0 |

## After (fail back + record)

1. Fail back per `dr.md` (re-replicate primary from DR, cut back in a window).
2. File the measured RPO/RTO + any gate miss as a finding; a missed gate is a DR
   regression — fix it (capacity, archive cadence, rebuild parallelism) before
   the next quarter, same discipline as a GameDay verify abort (SRE-01d).
