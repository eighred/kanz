# Chaos experiments (SRE-01c)

Fault injection that proves the platform's resilience primitives actually hold —
the experiments don't just *break* things, each one carries a **steady-state
hypothesis** asserting a specific designed behavior survives the fault.

## Tooling: Chaos Mesh

[Chaos Mesh](https://chaos-mesh.org/) (CNCF) — its `PodChaos`/`NetworkChaos`
CRDs cover every fault this suite needs (pod kill, partition, latency) with no
sidecar and no app changes, and it composes into the SRE-01d GameDay `Workflow`.
A mesh-based tool (Litmus, Gremlin) would add infra the platform doesn't run.

## The experiments

| File | Fault | Primitive under test | Hypothesis |
|---|---|---|---|
| `broker-kill.yaml` | kill a NATS / Kafka pod | EVT-17e DLQ + retry, reconnect | events not lost; DLQ catches terminal failures; lag drains on recovery |
| `network-partition.yaml` | isolate risk-engine from brokers | RISK-11 degraded mode | AsOf ages → serves last-known-good tagged stale; no crash; recovers on heal |
| `latency-injection.yaml` | slow the inference dependency | PRED-07 circuit breaker | breaker opens → degraded fallback; caller latency stays bounded |
| `pod-eviction.yaml` | kill / crash one service replica | replica redundancy (PRED-08 edge) | survivors serve; no SLO breach |

Each manifest embeds its hypothesis in `annotations.chaos.kanz.io/hypothesis`
and its expected during/after signals in comments. Blast radius is deliberately
small (`mode: one`, single namespace) — these are **staging/GameDay** artifacts,
never applied unscheduled to prod.

## Asserting "behaves as designed"

The fault is half the experiment; the assertion is the other half. `verify.sh`
queries the OBS-01 Prometheus for each hypothesis's signals and, critically,
checks the one **invariant common to every experiment**: no SLO is in
`SLOFastBurn` (SRE-01b) — i.e. the disruption fits inside the 28-day error
budget. Run it at three points:

```sh
PROM=http://localhost:9090 ./verify.sh broker-kill   # baseline (before)
kubectl apply -f broker-kill.yaml
PROM=http://localhost:9090 ./verify.sh broker-kill   # during: expect degraded signal
kubectl delete -f broker-kill.yaml
PROM=http://localhost:9090 ./verify.sh broker-kill   # after: expect recovery, exit 0
```

`verify.sh` exits non-zero if a must-hold invariant is violated (SLO fast-burn,
or data loss via `kanz_data_gap_missing_total`), so the SRE-01d GameDay workflow
can gate a run on it.

## Run a single experiment

```sh
kubectl apply -f network-partition.yaml          # start (has a duration; auto-clears)
kubectl describe networkchaos partition-risk-engine-brokers -n kanz-services
kubectl delete -f network-partition.yaml         # stop early
```

`PodChaos` without a `duration` re-injects until deleted; annotate
`experiment.chaos-mesh.org/pause=true` to hold it. The scheduled suite that
chains these with verification gates is SRE-01d (`gamedays/`).
