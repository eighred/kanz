---
title: <failure mode>
alert: <alertname or omit>
severity: page | ticket
slo: <service/slo or omit>
gameday: <infra/chaos/...yaml or omit>
---

# <failure mode>

## Symptom
What the on-call sees — the alert that fired, the user-visible effect.

## Confirm
PromQL / kubectl to verify this is the actual failure mode (and rule out
look-alikes). Prefer copy-pasteable queries.

## Impact
What is and isn't degraded. Which SLO's budget is burning.

## Mitigate
Stop the bleeding. The fastest safe action to restore steady state.

## Recover
Return to normal once the cause is cleared; how to confirm full recovery.

## Root-cause pointers
Where to look next (dashboards, logs, the primitive involved).
