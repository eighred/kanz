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

## Before (capture the baseline)

Record the replication lag at T0 — this is the **measured RPO**:

```sh
# Kafka (DR-01a): now − latest replicated heartbeat timestamp.
kubectl --context <dr> -n kanz-messaging exec kafka-0 -- \
  /opt/kafka/bin/kafka-console-consumer.sh --bootstrap-server kafka:9092 \
  --topic heartbeats --from-beginning --timeout-ms 5000 | tail -1
# Postgres (DR-01b): standby replay lag.
kubectl --context <dr> -n kanz-data cnpg status kanz-risk | grep -i lag
```

**Gate:** both lags < 1min, or the drill fails on RPO before you cut over.

## Run the failover (measure RTO)

Start a timer, then execute the DR-01d procedure:

```sh
time ( DR_CTX=<dr> ./infra/dr/failover.sh )   # wall-clock to services-Ready = RTO
```

Then flip a *test* DNS/LB weight (or a drill hostname) to DR and run a synthetic
transaction end-to-end (submit a query via the gateway, confirm a risk response).

**Gate:** wall-clock from declaration to a successful synthetic transaction
≤ 15min.

## Success criteria

| Objective | Measure | Target |
|---|---|---|
| RPO | max replication lag at T0 (Kafka + Postgres) | ≤ 1min |
| RTO | declaration → first successful synthetic txn | ≤ 15min |
| Integrity | post-failover audit chain verify (`/v1/audit/verify`) | 200 / verified |
| Data | no sequence gap (`kanz_data_gap_missing_total` flat) across cutover | 0 |

## After (fail back + record)

1. Fail back per `dr.md` (re-replicate primary from DR, cut back in a window).
2. File the measured RPO/RTO + any gate miss as a finding; a missed gate is a DR
   regression — fix it (capacity, archive cadence, rebuild parallelism) before
   the next quarter, same discipline as a GameDay verify abort (SRE-01d).
