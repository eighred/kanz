# INFRA-01e — autoscaler reaction validation

Proves the INFRA-01c KEDA autoscaling actually *reacts* to backpressure (the
PRED-14 premise: add workers to work the backlog down), the autoscaling analog
of the SRE-01c chaos hypotheses.

## Hypothesis

When the risk-engine consumer falls behind — `kanz_bus_pending_messages` for its
group climbs past the ScaledObject threshold — KEDA scales the Rollout above its
floor (3) toward `maxReplicaCount`; once the backlog drains, it scales back down
after the cooldown.

## Inject lag

Create a backlog faster than one consumer can drain it. Either:

- **Burst the source**: replay/produce a burst onto the risk-engine's input
  subjects so pending spikes, or
- **Throttle the consumer**: temporarily pin the Rollout to 1 replica
  (`kubectl argo rollouts set image …` is not needed — scale via the Rollout)
  and feed normal load so pending accumulates, then release.

```sh
# Watch the signal and the reaction side by side:
watch -n5 'kubectl -n kanz-services get scaledobject,hpa,rollout risk-engine'
```

## Assert (verify.sh)

During the injection:

```sh
PROM=http://localhost:9090 ./verify.sh autoscale
```

- `kanz_bus_pending_messages{group="risk-engine"}` is **elevated** (the signal),
  and
- the HPA's current replicas are **above the floor of 3** (the reaction) —
  the `autoscale` check asserts this.

## Success criteria

| | Measure | Expect |
|---|---|---|
| Signal | `sum(kanz_bus_pending_messages{group="risk-engine"})` during injection | > threshold (500) |
| Reaction | HPA current replicas | rises above 3, toward 12 |
| Recovery | replicas after backlog drains + cooldown (120s) | back to 3 |
| Invariant | `SLOFastBurn` | not firing — scaling absorbed the load |

A backlog that climbs without the replica count following is an autoscaling
regression (wrong metric, missing Prometheus scaler, RBAC) — fix it the same way
a GameDay verify abort is treated (SRE-01d).
