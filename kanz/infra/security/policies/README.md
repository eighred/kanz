# Authorization policies (AUTH-01b)

Deny-by-default authorization for the risk surface. The decision model lives in
`kanz/pkg/auth` (`authz.go`); this directory holds the **policy bundle** the
services load at runtime.

## Model

A decision is `Allow` only if **every** gate passes, checked isolation-first so
no role grant can ever widen access beyond the caller's tenant or portfolio:

1. **Authenticated** — a `Principal` is present (AUTH-01a).
2. **Tenant isolation (ABAC)** — the principal carries a tenant, and any
   tenant-scoped resource must belong to it. Cross-tenant is denied *before* RBAC
   even for the wildcard admin role. This is a structural invariant in code, **not**
   a policy-data grant — an isolation boundary (MT-01) must not be loosenable by
   editing a bundle.
3. **Portfolio scope (ABAC)** — if the principal carries a `portfolios` claim
   allow-list, the target portfolio must be on it; absent ⇒ any portfolio within
   the tenant.
4. **RBAC** — some role the principal holds must grant the action in this bundle
   (`"*"` grants all).

## Bundle format

`risk-authz.json` — RBAC role→action grants only (the ABAC dimensions are code
invariants, above):

```json
{ "roles": { "risk.reader": ["risk.read"], "risk.admin": ["*"] } }
```

Loaded via `auth.LoadPolicyFile(path)`; deployed as the `configmap.yaml`
ConfigMap mounted at `KANZ_AUTHZ_POLICY_FILE` (config-as-data, no rebuild to
change policy — same stance as the SEC-01d secret-file mounts).

## Why hand-rolled, not OPA/Cedar

The task named OPA/Cedar, but the decision here is structurally narrow (tenant
isolation + role→action grants + portfolio scope). Embedding the Rego runtime
(or Cedar) would be dependency bloat for that — the same call the gateway's
hand-rolled token-bucket rate limiter and REST transcoding made. The
`auth.Authorizer` **interface is the seam**: if policy outgrows this model, a
Rego/Cedar-backed `Authorizer` slots behind it without touching call sites.
