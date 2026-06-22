---
title: Broker outage (NATS / Kafka)
severity: page
slo: event-bus/delivery-success
gameday: infra/chaos/broker-kill.yaml
---

# Broker outage (NATS / Kafka)

## Symptom
`event-bus/delivery-success` burning and/or consumer lag climbing. Broker pods
not Ready in `kanz-messaging`.

## Confirm
```promql
sum(kanz_bus_consumer_lag)                                    # rising
sum(increase(kanz_bus_consume_total{result="error"}[10m]))   # DLQ-routed terminal failures
sum(increase(kanz_data_gap_missing_total[15m]))              # MUST stay 0 — gap = real loss
```
```sh
kubectl get pods -n kanz-messaging -l app=nats
kubectl get pods -n kanz-messaging -l app=kafka
```

## Impact
Events queue rather than drop (NATS JetStream / Kafka are durable). Downstream
consumers fall behind; the risk-engine ages into degraded mode (see
`broker-partition.md`). A non-zero `kanz_data_gap_missing_total` means the loss
escaped durability — escalate.

## Mitigate
1. Let the broker StatefulSet self-heal (pod reschedules, partition leadership
   moves). Do **not** delete PVCs — that's the durable log.
2. If a single pod is wedged, delete it to force reschedule:
   `kubectl delete pod <pod> -n kanz-messaging`.
3. Verify the DLQ for poisoned messages once recovered (`dlq.<subject>`); replay
   if the original failure was transient.

## Recover
Lag drains to baseline after consumers reconnect (they reconnect automatically).
Confirm `sum(kanz_bus_consumer_lag)` back to baseline and no new gaps.

## Root-cause pointers
DLQ/retry design: `pkg/bus/{consumer,retry}.go`. Broker topology:
`infra/nats/`, `infra/kafka/`. DATA-05 reconciliation confirms NATS↔Kafka stayed
matched through the outage.
