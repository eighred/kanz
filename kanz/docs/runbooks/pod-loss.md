---
title: Service replica lost / crashed
severity: ticket
slo: api-gateway/availability
gameday: infra/chaos/pod-eviction.yaml
---

# Service replica lost / crashed

## Symptom
A service pod (api-gateway, risk-engine) evicted, OOMKilled, or crash-looping.
Usually a non-event for clients — surfaces as a transient blip, not an outage.

## Confirm
```promql
sum(rate(kanz_gateway_requests_total{code=~"5.."}[5m]))   # ~flat if survivors absorb traffic
sum(rate(kanz_risk_recompute_total[5m]))                  # still advancing (another replica owns partitions)
```
```sh
kubectl get pods -n kanz-services -l app=<service>
kubectl describe pod <pod> -n kanz-services   # reason: Evicted / OOMKilled / Error
```

## Impact
If `replicas > 1`, survivors serve and no SLO breaches — this is expected
resilience. A crash-LOOP (not a one-off) on all replicas is a real outage:
escalate and treat the crash cause.

## Mitigate
1. One-off eviction/crash: let the scheduler recreate the pod. No action needed.
2. Crash-loop: inspect logs (`kubectl logs <pod> -n kanz-services --previous`).
   OOMKilled → raise memory limits; config/secret error → check the SEC-01d CSI
   mounts and env (`*_FILE` secret paths).

## Recover
Replacement pod reaches Ready and rejoins the consumer group / serving pool.
Confirm no `SLOFastBurn` fired and throughput is unchanged.

## Root-cause pointers
Rollout/replica config: `infra/deploy/risk-engine-rollout.yaml`. The CICD-01e
canary assumes exactly this replica redundancy.
