---
title: Inference slow → circuit breaker open
severity: ticket
slo: risk-engine/recompute-latency
gameday: infra/chaos/latency-injection.yaml
---

# Inference slow → circuit breaker open

## Symptom
Predictions returning DEGRADED with `degraded_reason=circuit_open` (or
`no_cached_prediction`); the inference dependency is slow/timing out, but the
caller's own latency stays bounded.

## Confirm
```promql
histogram_quantile(0.99, sum(rate(kanz_risk_recompute_duration_seconds_bucket[5m])) by (le))  # bounded — breaker is shedding
```
Degraded predictions surface via the PRED-02 `degraded_reason` on the prediction
envelope / kanz-py inference logs (`circuit_open`).

## Impact
The PRED-07 breaker is doing its job: it opened after consecutive timeouts and
calls now fast-fail to the degraded fallback instead of blocking. Predictions
are degraded (last-known/zero-value tagged), but the caller is **not** stalled —
latency cascade prevented.

## Mitigate
1. This is the inference service's problem — triage **it**, not the caller. Check
   inference pod health/saturation (CPU, model load, GC) in `kanz-services`.
2. Scale or restart the inference workload if saturated. The breaker half-opens
   automatically after `BreakerCooldown` and closes once calls succeed again.

## Recover
Once inference latency recovers, the breaker closes and predictions return to
NORMAL (no `degraded_reason`). Confirm degraded predictions stop.

## Root-cause pointers
Breaker + per-call timeout + fallback: `internal/prediction/sync_client.go`
(`CircuitBreaker`, `BreakerThreshold`, `BreakerCooldown`). Degraded reasons:
PRED-02 §2.
