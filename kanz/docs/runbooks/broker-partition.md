---
title: Broker partition → risk-engine degraded mode
severity: page
slo: market-data/freshness
gameday: infra/chaos/network-partition.yaml
---

# Broker partition → risk-engine degraded mode

## Symptom
Risk query responses tagged **stale** (RISK-11 QualityFlags); staleness lag
climbing while the risk-engine itself stays Ready (not crash-looping).

## Confirm
```promql
max(kanz_data_staleness_lag_seconds)        # > 30s (FRESH→DEGRADED), > 5m (full degraded)
```
```sh
kubectl get pods -n kanz-services -l app=risk-engine          # Ready, not CrashLoop
kubectl exec -n kanz-services <risk-engine-pod> -- nc -vz nats.kanz-messaging 4222   # reachability
```

## Impact
The engine serves **last-known-good** results tagged stale rather than erroring
— degraded mode working as designed. Decisions are made on an aging view; the
older the AsOf, the less trustworthy. This is graceful, not an outage.

## Mitigate
1. This is a *network/connectivity* problem, not a compute one — do not restart
   the engine (restart loses nothing but doesn't fix the partition and resets
   the warm cache).
2. Restore connectivity between `kanz-services` and `kanz-messaging`
   (NetworkPolicy, CNI, broker reachability). Confirm with the `nc` check above.

## Recover
On heal the consumer reconnects and replays the backlog; AsOf catches up and
mode returns to FRESH. Confirm `max(kanz_data_staleness_lag_seconds)` back under
the 30s freshness budget and responses no longer tagged stale. No data gap
(`kanz_data_gap_missing_total` flat) — events were delayed, not lost.

## Root-cause pointers
Degraded-mode design + thresholds (`DefaultFreshnessBudget` 30s,
`DefaultDegradedThreshold` 5m): `internal/risk/degraded.go`.
