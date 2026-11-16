# Progressive delivery + automated rollback (CICD-01e)

Canary rollouts for the Kanz services via [Argo Rollouts](https://argo-rollouts.readthedocs.io/),
with metric-gated promotion and **automatic rollback** on regression.

## Why Argo Rollouts (replica canary), not Flagger/mesh traffic-split

risk-engine is a NATS consumer + gRPC query service, not an HTTP workload behind
an ingress, so weight-based *traffic* splitting needs a service mesh it doesn't
have. Argo Rollouts' **replica-based canary** shifts a fraction of *pods* to the
new revision and gates each step on a Prometheus `AnalysisRun` — no mesh
required, and the analysis is the real safety mechanism regardless of routing.

## How it works

`risk-engine-rollout.yaml` replaces the Deployment with a `Rollout` whose
canary `steps` are: 20% → pause 60s → **analysis** → 50% → pause 120s →
**analysis** → 100%. Each analysis runs `analysis-template.yaml` against the
**canary pods only** (scoped by the `rollouts-pod-template-hash`):

- recompute success-rate ≥ 99%
- recompute p99 latency ≤ 500ms (the ORCH-01f budget)

`failureLimit: 0` — one bad measurement aborts. **Abort = rollback**: Argo
Rollouts scales the canary ReplicaSet to zero and leaves the stable revision
serving. No human in the loop for the unhappy path.

The pod composes the rest of the platform: the **CICD-01c/d signed image**
(admission verifies the cosign signature, `infra/security/admission`), the
**SEC-01a SVID** (SPIFFE CSI socket + `risk-engine` ServiceAccount), and the
**SEC-01d CSI secret** for the DB DSN (`RISK_ENGINE_DATABASE_URL_FILE`, never a
plaintext env).

The risk engine exports the `kanz_risk_*` series consumed by this analysis.
Prometheus must scrape the canary labels and answer both queries. The template
handles explicitly empty or idle estates using inventory, queue and cumulative
recompute evidence; missing series and provider errors do not establish success.
The presence of these manifests does not prove a cluster rollout.

## Operate

```sh
# Install the controller once per cluster (pinned chart).
kubectl apply -f analysis-template.yaml -f risk-engine-rollout.yaml

# A new image (GitOps bumps spec.template.spec.containers[0].image) triggers the
# canary automatically. Watch it:
kubectl argo rollouts get rollout risk-engine -n kanz-services --watch

# Manual controls (the automated path needs none of these):
kubectl argo rollouts promote risk-engine -n kanz-services   # skip remaining pauses
kubectl argo rollouts abort   risk-engine -n kanz-services   # force rollback
kubectl argo rollouts undo    risk-engine -n kanz-services   # roll back to prior revision
```

## Scope and operational limits

The risk engine owns the canary Rollout; schema-registry uses a plain Deployment.
The base manifests are deployment inputs, while the Tokyo overlay and its
reviewed release lock select immutable runtime digests. The analysis checks
recompute success and latency; it has no bus consumer-lag gate. Separate KEDA
scaling does not add that promotion criterion. Release and rollback proof is
tracked in [#1098](https://github.com/eighred/kanz/issues/1098); broader
deployment changes belong in Issues rather than a local follow-up list.

## Phase-7 read services (SVCWIRE-01a)

`wealth-deploy.yaml`, `datamaster-deploy.yaml`, `alternatives-deploy.yaml`, and
`copilot-deploy.yaml` make the Phase-7 services deployable. They are
**HTTP read surfaces on :8080**, so they ship as plain `Deployment`s — not the
risk-engine canary `Rollout` — keeping the SEC-02a hardening + the SEC-01a SPIFFE
SVID (the gateway reaches them over mTLS, SVCWIRE-01c).

They are **not** stateless, and they no longer boot without a database: wealth,
datamaster and alternatives each require their `*_DATABASE_URL` and **refuse to
start (exit 2) without it** (#261), so the DB CSI volume in each is load-bearing
rather than optional. Each carries a `*_ALLOW_EPHEMERAL_*` escape hatch for a
laptop, unset in every shipped manifest; a deployment that sets one must drop its
replica count to 1, because the in-memory stores are per-pod maps behind a
load-balancing consumer group. copilot is the exception — it holds no store. The `workloads` ApplicationSet component recurses this directory,
so adding the file *is* the GitOps registration — no per-service Argo Application.

copilot boots safely closed: with no `COPILOT_POLICY_PATH` its authorizer denies
all, and `COPILOT_MODEL_ID` is the version knob (default `claude-fable-5`); the
real Claude client + governed mTLS client wire at the composition root
(PARITY-04a/b). It also **refuses to start (exit 2) without `COPILOT_PROVIDER`**
(#179) — which model vendor answers is chosen at runtime among the adapters the
image linked, and is deliberately not defaulted. Changing that value also needs
that vendor's credential mounted, or the pod exits 2 at boot naming the mount. The progressive-delivery `Rollout` + PDB + KEDA for these
services come in SVCWIRE-01d; edge auth + routes in SVCWIRE-01b.

## Autoscaling + availability (INFRA-01c/d)

The same directory carries the risk-engine resilience layer (GitOps-synced):

| File | Task | What |
|---|---|---|
| `risk-engine-scaledobject.yaml` | INFRA-01c | KEDA scales the Rollout on `kanz_bus_pending_messages` (OBS-01c) — the PRED-14 "add workers to drain the backlog" premise; floor 3, ceiling 12 |
| `availability.yaml` | INFRA-01d | PodDisruptionBudget (minAvailable 2) + namespace ResourceQuota + LimitRange |
| `risk-engine-rollout.yaml` (topologySpreadConstraints) | INFRA-01d | replicas spread across AZs so a zone loss (INFRA-01e) still serves |

KEDA drives the Rollout's replica count while the canary analysis still gates new
revisions — scaling and progressive delivery compose. Validated by the INFRA-01e
`az-kill` experiment + `autoscale-validation.md` (in `infra/chaos`).
