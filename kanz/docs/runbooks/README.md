# Runbooks-as-code (SRE-01d)

Operational runbooks for the Kanz platform, kept **in the repo, in source
control** — reviewed, diffed, and versioned with the code whose failures they
describe (not a wiki that rots). Each runbook is one failure mode: what fired,
what's happening, how to confirm, how to mitigate, how to recover.

## Structure

Every runbook carries YAML frontmatter so it's machine-linkable:

```yaml
---
title: <human title>
alert: <SLOFastBurn|...>      # the alertname this runbook resolves (optional)
severity: page | ticket
slo: <service/slo>            # the SLO at risk (optional)
gameday: <experiment file>    # the SRE-01c experiment that rehearses this
---
```

Alerts link here via a `runbook_url` annotation pointing at the file, so a page
carries its runbook. The SRE-01d GameDay (`infra/chaos/gamedays/`) rehearses
exactly these scenarios — each runbook names the experiment that exercises it,
so the drill is the runbook's live test.

## The runbooks

| File | Scenario | Rehearsed by |
|---|---|---|
| `slo-error-budget-burn.md` | an SLO burn-rate alert is firing | every experiment |
| `broker-outage.md` | NATS/Kafka pod or quorum loss | `broker-kill.yaml` |
| `broker-partition.md` | risk-engine isolated → degraded mode | `network-partition.yaml` |
| `inference-latency.md` | inference slow → circuit breaker open | `latency-injection.yaml` |
| `pod-loss.md` | a service replica lost/crashed | `pod-eviction.yaml` |
| `dr.md` | region failover (disaster recovery) | `infra/dr/failover.sh` |
| `dr-drill.md` | quarterly live-failover DR drill | `infra/dr/failover.sh` |
| `_template.md` | copy to author a new runbook | — |

## Keeping them honest

A runbook is only trustworthy if its steps actually work. The GameDay schedule
(SRE-01d) re-runs the matching fault weekly; if a runbook's confirm/mitigate
steps drift from reality, the drill surfaces it. Treat a GameDay surprise as a
runbook bug — fix the doc in the same PR.
