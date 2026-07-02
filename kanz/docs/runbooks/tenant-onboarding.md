---
title: Tenant onboarding golden path
severity: ticket
gameday: infra/onboarding/provision-tenant.sh
---

# Tenant onboarding golden path (PARITY-06e)

Provision a new institutional tenant end-to-end — across every MT-01 isolation
layer, its AUTH-01b policy bundle, per-tenant quotas, data-source binding, and a
first portfolio — then prove isolation holds before handover. The whole path is
scripted (`infra/onboarding/provision-tenant.sh`); this runbook is the operator's
guide to running and verifying it.

## One command

```sh
cd infra/onboarding
TENANT=acme ADMIN_SUBJECT=alice@acme.com ./provision-tenant.sh
```

Each step is idempotent and re-runnable (`STEP=<step>` runs one), so a partial
provision resumes rather than restarts.

## What it does, layer by layer

| Step | Layer | Action |
|---|---|---|
| storage | MT-01d | Verify Postgres RLS + FORCE RLS is active — the tenant's rows are scoped by `app.tenant_id` on the shared clusters (no per-tenant DB) |
| broker | MT-01c | Confirm envelope `tenant_id` stamping (the `bus.Producer` requires a non-empty Tenant); events share subjects, isolation is by tenant claim |
| policy | AUTH-01b | Install the tenant's role→permission bundle (the `risk-authz.json` policy-as-data shape) as a ConfigMap; map the IdP admin group → `risk.admin` |
| quota | MT-01e | Upsert the tenant into the gateway `QuotasFile` (its own rate/burst/in-flight caps, not the shared default) |
| seed | data-source | Bind feed entitlements and seed a first portfolio (`test/load/seed`) so the read path serves real data day one |
| verify | AUTH-01 | The tenant reads its own portfolio (200) **and** a cross-tenant probe is denied (403/404) — the gate that must hold before handover |

## Portfolio scoping (per-user, within a tenant)

Tenant isolation is the outer boundary; within a tenant, a principal can be
further restricted to specific portfolios via the `portfolios` claim
(`auth.ClaimPortfolios`). Grant an analyst access to a subset by populating that
claim in the IdP token mapping — the `PolicyAuthorizer` denies any portfolio not
in the allow-list even inside the correct tenant. Leave the claim empty for
tenant-wide access (the admin/reader default).

## The isolation gate is the acceptance bar

The `verify` step is not optional — a provision that leaks cross-tenant is worse
than no provision. It reproduces the MT-01g isolation contract for the new tenant:
its principal reads its own data, and a probe carrying a different tenant is
denied end-to-end at the gateway. If `verify` fails, the tenant is NOT handed
over; chase the failing layer (`STEP=policy`/`quota`/`storage` re-run) before
retrying.

## Handover checklist

- [ ] `provision-tenant.sh` completed with `isolation holds`.
- [ ] Admin subject can authenticate through the IdP and is mapped to `risk.admin`.
- [ ] First portfolio queryable via the gateway (exposure + measures return data).
- [ ] Per-tenant quota confirmed in the gateway ConfigMap.
- [ ] Onboarding recorded as evidence for SOC 2 CC6.x (see `soc2-evidence.md`).
