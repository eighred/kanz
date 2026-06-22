# INFRA-01b — GitOps delivery (Argo CD)

Every in-cluster workload is delivered by Argo CD from git — no `kubectl apply`
in anger. One bootstrap, then the platform manages itself.

| File | Role |
|---|---|
| `bootstrap.yaml` | the single Application you apply once; it syncs this directory (project + applicationset) so Argo CD manages its own config from git |
| `projects.yaml` | the `kanz` AppProject — allowlist of repo, namespaces, and cluster-scoped kinds (deny-by-default posture) |
| `applicationset.yaml` | matrix ApplicationSet: one Application per (environment × component) |

## App-of-apps + per-environment overlays

The matrix ApplicationSet *is* the app-of-apps: it fans out
`environments × components` into one Application each (`kanz-{env}-{component}`).

- **Components** are the in-cluster k8s workloads — the NATS + Kafka data planes
  and the delivery layer (`infra/deploy`: the risk-engine Rollout, the INFRA-01c
  KEDA ScaledObject, the INFRA-01d availability resources).
- **Environments** are the per-environment dimension. Each carries its own
  destination cluster and an env label, so `primary` and `dr` (the DR-01
  active/passive pair) sync the same component bases to different clusters.

Adding a component or an environment is a one-line list entry.

## Boundaries (what is *not* here)

- The **Terraform foundation** (INFRA-01a) — cloud resources, not k8s objects.
- **Prometheus rules / Grafana dashboards** (`infra/observability`) — shipped as
  their own PrometheusRule/ConfigMap wrapping, not raw-synced.
- **Templated/operational manifests** (`tenancy.yaml`, `*-job.yaml`, smoke
  tests) — applied by their own tooling (tenantctl, the DR runbook), excluded
  from auto-sync so a one-shot Job isn't perpetually re-created.
- The **DR Postgres split** (primary clusters vs replica clusters) follows the
  environment branch / DR runbook, not a single synced path.

## Bootstrap

```sh
# Register the destination clusters first (once):
argocd cluster add <primary-context>
argocd cluster add <dr-context>
# Then the one apply:
kubectl apply -f bootstrap.yaml
argocd app list   # kanz-primary-*, kanz-dr-* appear and sync
```

`automated.selfHeal` + `prune` mean drift is corrected and deletions in git
propagate — git is the source of truth.
