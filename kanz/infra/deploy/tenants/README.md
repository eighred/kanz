# Per-tenant compute (MT-02)

An onboarded tenant (`tenantctl.sh onboard`) gets a NATS account and Kafka
topics — but no process to serve it. This directory is where that gap closes,
one rendered manifest per tenant per service.

**Scope is declared in `internal/tenantgen/services.go`, not here** — that table
is the single source of truth, and the arch guards read it rather than a list in
prose. Today it holds `oms` and `archiver`.

**Why more than the OMS (#637).** A NATS account's subjects are physically
invisible outside it. A tenant given an order path and nothing else publishes
every FACT it produces — the accepted order, the fills, `risk.position.changed`
— into an account no platform service is a member of. Orders in, nothing out,
and every service Ready throughout, because "this tenant produced no events" and
"this tenant's events cannot reach me" are the same observable state.

The archiver moved first because it is the DR path, and CLAUDE.md's one
sequencing rule that outlives any issue is that real orders must never be placed
against a store that may not be backed up. It also could not be fixed any other
way: exporting the tenant's FACTs into `__system__` would meet a platform
archiver pinned to `ARCHIVER_TENANT=__system__`, and `internal/topic.For` REFUSES
an envelope whose tenant_id differs rather than misfiling it into another
tenant's Kafka prefix — an infinite NACK loop, not an archive. `ARCHIVER_TENANT`
and `topic.Qualify` were built for exactly this and had never been wired.

`accounting`, `risk-engine` and `audit` have NOT moved; #668 carries them, and
`test/arch/tenant_bridge_parity_test.go` holds one named exemption per role so
the gap cannot spread to a second tenant silently.

Adding a service here means adding it to `internal/tenantgen.Services` and
teaching `Render` any kind its base declares that the four supported ones do not
cover — not inventing a second mechanism.

**Two prerequisites per rendered service, and they fail differently.** The
generator prints both as it writes each manifest:

- its SPIFFE ID (`spiffe://kanz.internal/ns/kanz-services/sa/<service>-<tenant>`)
  must be admitted by the tenant's account in `infra/nats/tenancy.yaml`, or the
  pod authenticates and maps to NO account — it can neither publish nor
  subscribe (SEC-M3);
- a service marked `KafkaProducer` needs that same principal granted the
  tenant's PREFIXED Kafka ACL. `tenantctl.sh`'s `COMPUTE_KAFKA_SAS` does it;
  without it the pod comes up Ready and NACKs every event forever, because Kafka
  auto-create is disabled and it cannot create the `{tenant}.` topics it maps to.

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

For each of a base's four objects (ServiceAccount, Service, Deployment,
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
- sets the declared container's `*_TENANT` env var to `<tenant>` (by
  container + env-var name from the `Service` entry, not a positional index, so
  it survives the base reordering or growing its env list). A base with no such
  env var is a hard error: a per-tenant deployment that still reads `__system__`
  is the defect this generator exists to close, wearing the tenant's name.

See `internal/tenantgen/render.go`'s package doc for the determinism
contract the drift guard depends on.

## Adding a real tenant

1. `go run ./cmd/kanz-tenantgen -tenant <tenant>` from `kanz/` (or let
   `provision-tenant.sh`'s `compute` step do it). This writes one
   `infra/deploy/tenants/<tenant>/<service>-<tenant>.yaml` per declared service.
2. Add each printed SPIFFE ID to the tenant's account in
   `infra/nats/tenancy.yaml` — the pods cannot reach the broker without it, per
   SEC-M3 — and re-run `tenantctl.sh onboard` (or its `kafka` step) so the
   `KafkaProducer` principals get the tenant's prefixed ACL.
3. `test/arch/tenant_compute_test.go` requires every
   `infra/deploy/tenants/<tenant>/` to have a matching `tenancy.yaml` account
   (and vice versa), one non-drifted manifest per declared service, no file the
   generator would not produce, and agreement between `tenantgen`'s
   `KafkaProducer` set and `tenantctl.sh`'s `COMPUTE_KAFKA_SAS`.
   `test/arch/tenant_bridge_parity_test.go` requires a return path for every
   consumer role. Run `go test ./test/arch/... -count=1` before committing.
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
