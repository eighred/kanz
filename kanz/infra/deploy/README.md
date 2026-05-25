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

> Metric dependency: the analysis queries the OBS-01 `kanz_risk_*` Prometheus
> series (OBS-01c). Until OBS-01 ships them the queries are the contract the
> canary asserts; wire OBS-01 before relying on auto-rollback in anger.

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

## Follow-ups

- schema-registry gets the same pattern (its own Rollout + analysis) — risk-engine
  is the canonical first tenant here.
- Pin the image by digest (GitOps) instead of `:latest`.
- Add a bus consumer-lag analysis metric (OBS-01c) so a canary that silently
  stops consuming also aborts, not just one that errors/slows.
