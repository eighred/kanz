# Per-tenant compute (MT-02)

An onboarded tenant (`tenantctl.sh onboard`) gets a database role, a NATS
account, and Kafka topics — but no process to serve it. This directory is
where that gap closes for the OMS, one overlay per tenant.

**Scope: OMS only.** `archiver` and `market-ingest` stay platform-level on
`__system__` (see `KANZ_BRAIN.md` / archiver's manifest). Extending this
pattern to another service means adding another base-referencing overlay
alongside `oms-deploy.yaml`'s, not inventing a second mechanism.

## Why this directory, and not `infra/deploy/tenants/`

`infra/gitops/applicationset.yaml`'s `workloads` component points
`directory.recurse: true` at `kanz/infra/deploy`. Anything under that tree —
including a stray `kustomization.yaml` — is swept up and synced as a raw
manifest. A `kind: Kustomization` object has no corresponding Kubernetes API
type, so ArgoCD would try and fail to apply it as a resource. Tenant overlays
therefore live at `kanz/infra/tenants/<tenant>/`, outside that tree, and are
onboarded to ArgoCD via their own generator (see `applicationset.yaml`,
`tenants` list generator).

## How each overlay works

Each `kanz/infra/tenants/<tenant>/kustomization.yaml`:

- references the **real** base — `resources: [../../deploy/oms-deploy.yaml]`
  — never a copy. A new env var or field added to the base is inherited by
  every tenant overlay for free; a copy would silently drift the moment the
  base changes and nothing would compare the two.
- `nameSuffix: "-<tenant>"` renames every object the base declares
  (ServiceAccount, Service, Deployment, PodDisruptionBudget: all currently
  named `oms`) to `oms-<tenant>`. Because the base already declares the
  `ServiceAccount oms` object (not just a dangling reference to one),
  kustomize's built-in name-reference transformer also rewrites the
  Deployment's `serviceAccountName: oms` to `serviceAccountName: oms-<tenant>`
  automatically — no separate patch needed for that field. This is verified
  by rendering (see below), not assumed.
- `labels: [{pairs: {tenant: <tenant>}, includeSelectors: true}]` — **load
  bearing, not decorative.** The base's Service selector, Deployment
  `spec.selector`/pod-template labels, PodDisruptionBudget selector, and
  `topologySpreadConstraints` label selector are all `app: oms` — identical
  across every tenant, because every overlay pulls from the same base file.
  All tenant OMS pods land in the **same** `kanz-services` namespace (this is
  deliberate — see the plan's ground truth #5: the per-tenant boundary here is
  the ServiceAccount/SPIFFE identity, not a namespace). Without
  `includeSelectors: true`, tenant A's Service would load-balance across
  tenant B's pods too, and tenant A's PodDisruptionBudget would count tenant
  B's replicas toward its own `minAvailable`. Stamping `tenant: <tenant>` onto
  every selector as well as every object's labels is what keeps each tenant's
  Service/Deployment/PDB/topology-spread scoped to its own pods only.
- patches `OMS_TENANT` to `<tenant>` by container name + env-var name (merge
  keys), not a positional index, so it survives the base reordering or
  growing its env list.

## Adding a real tenant

1. Copy `_example/` to `kanz/infra/tenants/<tenant>/`.
2. In the new `kustomization.yaml`, replace every `_example` with `<tenant>`
   (the `nameSuffix`, the `labels.pairs.tenant` value, and the `OMS_TENANT`
   patch value).
3. Render it (`kubectl kustomize --load-restrictor=LoadRestrictionsNone
   kanz/infra/tenants/<tenant>/` — see the ArgoCD prerequisite below for why
   the flag is needed) and confirm the names, `OMS_TENANT`, and
   `serviceAccountName` all say `<tenant>`.
4. Add the tenant's SPIFFE ID
   (`spiffe://kanz.internal/ns/kanz-services/sa/oms-<tenant>`) to its account
   in `infra/nats/tenancy.yaml` (`provision-tenant.sh`'s `compute` step or
   `tenantctl.sh` documents this — the pod cannot reach the broker without
   it, per SEC-M3).
5. `test/arch/tenant_compute_test.go` requires every `infra/tenants/<tenant>/`
   to have a matching `tenancy.yaml` account, and vice versa — run
   `go test ./test/arch/... -count=1` before committing.
6. **Commit to `main`.** ArgoCD's `tenants` generator (in
   `applicationset.yaml`) picks up the new directory and syncs it. There is no
   apply step — see the next section for why.

## Never `kubectl apply` this

`infra/deploy/` (and, by the same generator pattern, `infra/tenants/*`) is
raw/kustomize-synced by ArgoCD with `syncPolicy.automated: { prune: true,
selfHeal: true }`. An out-of-band `kubectl apply` of a tenant's OMS manifests
would come up, and then be **pruned within minutes** because it does not
exist in git. Provisioning **emits** the overlay; **the commit** is what
deploys it. `provision-tenant.sh`'s `compute` step refuses to apply anything
for exactly this reason and prints the commit instruction instead.

## ArgoCD prerequisite: `kustomize.buildOptions`

`resources: [../../deploy/oms-deploy.yaml]` reaches **outside** each tenant
overlay's own directory to a raw file with no `kustomization.yaml` of its own.
Kustomize's default `LoadRestrictionsRootOnly` refuses this — reproduced
locally:

```
error: accumulating resources: accumulation err='accumulating resources from
'../../deploy/oms-deploy.yaml': security; file '.../kanz/infra/deploy/oms-deploy.yaml'
is not in or below '.../kanz/infra/tenants/_example'': must build at directory: ...
```

The fix is `--load-restrictor=LoadRestrictionsNone` (used for the render in
this README and below). ArgoCD's repo-server applies the same default
restriction to every Kustomize-sourced Application, controlled by the
**cluster-wide** `argocd-cm` ConfigMap key `kustomize.buildOptions`. That
ConfigMap is not part of this repo (ArgoCD's own installation is managed
outside `kanz/`, the same way cluster registration is — see
`applicationset.yaml`'s "Register these cluster names in Argo CD" comment for
the precedent). **Before the first tenant Application syncs, confirm
`argocd-cm` sets `kustomize.buildOptions: "--load-restrictor
LoadRestrictionsNone"`** — without it, every tenant Application will fail to
sync with the exact error above, which is a Degraded/error Application, not a
silent no-op.

The two options that would avoid this external dependency are both closed:
copying the base drifts (forbidden — see the plan's global constraints), and
giving `oms-deploy.yaml` its own `kustomization.yaml` would put a
`kind: Kustomization` object inside the raw-synced `infra/deploy/` tree (see
"Why this directory" above).
