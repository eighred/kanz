# Scheduled GameDay suite (SRE-01d)

Turns the SRE-01c experiments into a **supervised, recurring drill** that
re-proves resilience on a schedule, gated on verification, with a runbook per
scenario.

| File | What |
|---|---|
| `gameday-workflow.yaml` | Chaos Mesh `Workflow` — runs the four faults serially, each followed by a `verify.sh` gate; a violated invariant aborts the drill |
| `schedule.yaml` | Chaos Mesh `Schedule` — instantiates the workflow weekly (Wed 14:00 UTC, on-call staffed) |

Runbooks for every scenario the drill exercises live in
[`kanz/docs/runbooks/`](../../../docs/runbooks/) — the GameDay is the runbooks'
live test.

## One-time setup

The verify gates run `infra/chaos/verify.sh` from a ConfigMap:

```sh
kubectl create configmap chaos-verify -n kanz-services \
  --from-file=verify.sh=../verify.sh
```

Install Chaos Mesh (once per cluster) per its chart; the CRDs used here are
`PodChaos`, `NetworkChaos`, `Workflow`, `Schedule`.

## Run a GameDay now (ad-hoc)

```sh
kubectl apply -f gameday-workflow.yaml
kubectl get workflow kanz-gameday -n kanz-services -w
# Follow along in the runbooks; a failed verify-* node aborts the drill.
```

## Install the weekly schedule

`schedule.yaml` is a thin cron wrapper; its workflow body is filled from
`gameday-workflow.yaml` at apply time so there's a single source of truth:

```sh
yq eval-all 'select(fi==0).spec.workflow = select(fi==1).spec | select(fi==0)' \
  schedule.yaml gameday-workflow.yaml | kubectl apply -f -
```

Pause during a change freeze (SRE-01a error-budget policy):

```sh
kubectl patch schedule kanz-gameday-weekly -n kanz-services \
  --type merge -p '{"spec":{"paused":true}}'
```

## After a GameDay

- A `verify-*` abort = a resilience regression. Treat it as a SEV: the primitive
  it tests (DLQ, degraded mode, breaker, replica redundancy) did not hold.
- A runbook step that didn't match reality = a runbook bug; fix it in the same
  PR. Drift between runbook and system is the failure the drill exists to catch.
