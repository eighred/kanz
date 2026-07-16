# Per-tenant compute (MT-02)

An onboarded tenant (`tenantctl.sh onboard`) gets a database role, a NATS
account, and Kafka topics — but no process to serve it. This directory is
where that gap closes for the OMS, one rendered manifest per tenant.

**Scope: OMS only.** `archiver` and `market-ingest` stay platform-level on
`__system__` (see `KANZ_BRAIN.md` / archiver's manifest). Extending this
pattern to another service means teaching `internal/tenantgen.Render` that
service's kinds and selectors, not inventing a second mechanism.

## Why this directory, and not a kustomize overlay

An earlier version of this design put a `kustomization.yaml` per tenant at
`kanz/infra/tenants/<tenant>/`, referencing `../../deploy/oms-deploy.yaml` as
its base so a change to the base was inherited "for free." It was reverted:
rendering that overlay requires `--load-restrictor=LoadRestrictionsNone`
(kustomize refuses, by default, to read a file from outside the overlay's own
directory), which means either ArgoCD's cluster-wide `argocd-cm` carries that
flag — a security relaxation applied to every Application, in a repo that
does not manage `argocd-cm` — or CI renders with the flag while ArgoCD syncs
with a different setting, which is the same "two places, nothing comparing
them" defect this repo has spent a week retiring. Worse: the guard that
existed then checked directory *structure*, not a *render* — it passed while
`kubectl kustomize` on the exact same tree failed outright, so CI was green
while zero tenants could actually deploy.

**What replaced it:** a committed, plain-YAML manifest here — rendered by
`internal/tenantgen.Render`, never hand-edited — under
`infra/deploy/tenants/<tenant>/oms-<tenant>.yaml`. `infra/deploy/` is already
raw-synced by the ApplicationSet's `workloads` component
(`infra/gitops/applicationset.yaml`: `directory.recurse: true`,
`prune: true`, `selfHeal: true`). A plain manifest under that tree needs no
new Application, no kustomize, and no ArgoCD-side flag: what is committed is
exactly what deploys, the same way every other file under `infra/deploy/`
already does.

A committed copy of the base would normally drift the moment the base
changes, with nothing comparing the two — but this one is safe, because
nothing ever hand-maintains it. `internal/tenantgen.Render(base, tenant)` is
the ONE generator, and it is the ONLY thing that ever writes this file:
`cmd/kanz-tenantgen` calls it (used by `provision-tenant.sh`'s `compute`
step), and `test/arch/tenant_compute_test.go`'s drift guard calls the exact
same function to re-render every committed manifest from the LIVE base on
every CI run, diffing the whole document byte-for-byte. A new env var or
volume added to `oms-deploy.yaml` becomes a CI failure here, not a silently
divergent tenant.

## What Render does

For each of the base's four objects (ServiceAccount, Service, Deployment,
PodDisruptionBudget), `internal/tenantgen.Render`:

- renames `metadata.name` from `oms` to `oms-<tenant>` (and, for the
  Deployment, rewrites `spec.template.spec.serviceAccountName` to match),
- stamps `tenant: <tenant>` onto `metadata.labels` on every object, **and**
  onto every selector the per-tenant isolation boundary depends on: the
  Service's `spec.selector`, the Deployment's `spec.selector.matchLabels` +
  pod-template labels + `topologySpreadConstraints` label selector, and the
  PodDisruptionBudget's `spec.selector.matchLabels`. This is **load-bearing,
  not decorative** — the base's selectors are all `app: oms`, identical
  across every tenant, because every manifest renders from the same base
  file. All tenant OMS pods land in the same `kanz-services` namespace (this
  is deliberate — the per-tenant boundary here is the ServiceAccount/SPIFFE
  identity, not a namespace). Without the `tenant:` stamp, tenant A's Service
  would load-balance across tenant B's pods too, and tenant A's
  PodDisruptionBudget would count tenant B's replicas toward its own
  `minAvailable`.
- sets the `oms` container's `OMS_TENANT` env var to `<tenant>` (by
  container + env-var name, not a positional index, so it survives the base
  reordering or growing its env list).

See `internal/tenantgen/render.go`'s package doc for the determinism
contract the drift guard depends on.

## Adding a real tenant

1. `go run ./cmd/kanz-tenantgen -tenant <tenant>` from `kanz/` (or let
   `provision-tenant.sh`'s `compute` step do it). This writes
   `infra/deploy/tenants/<tenant>/oms-<tenant>.yaml`.
2. Add the tenant's SPIFFE ID
   (`spiffe://kanz.internal/ns/kanz-services/sa/oms-<tenant>`) to its account
   in `infra/nats/tenancy.yaml` (the generator prints this instruction) — the
   pod cannot reach the broker without it, per SEC-M3.
3. `test/arch/tenant_compute_test.go` requires every
   `infra/deploy/tenants/<tenant>/` to have a matching `tenancy.yaml` account
   (and vice versa), and that the committed manifest is not drifted from the
   live base — run `go test ./test/arch/... -count=1` before committing.
4. **Commit to `main`.** The ApplicationSet's `workloads` component
   (`applicationset.yaml`) already syncs everything under `infra/deploy/`;
   there is no separate Application, and no apply step.

## Never `kubectl apply` this

`infra/deploy/` is raw-synced by ArgoCD with `syncPolicy.automated:
{ prune: true, selfHeal: true }`. An out-of-band `kubectl apply` of a
tenant's OMS manifest would come up, and then be **pruned within minutes**
because it does not exist in git. `provision-tenant.sh`'s `compute` step
refuses to apply anything for exactly this reason and prints the commit
instruction instead.
