# Tenant lifecycle (MT-01f)

One workflow that onboards/offboards a tenant across **every** isolation
boundary the multi-tenancy epic built, from a single tenant definition. Without
this, standing up a tenant means hand-applying NATS, Kafka, Postgres, gateway,
and SPIRE changes in the right order and never drifting — costly and
error-prone. `tenantctl.sh` makes it one idempotent command.

## What it provisions, in order

The order is a dependency chain — the SVID minted in step 1 is the credential
every later step keys on.

| # | Boundary | Resource | Built in |
|---|---|---|---|
| 1 | Identity | `tenant-<name>` namespace (SPIFFE-enabled) + ServiceAccounts ⇒ SVIDs | SEC-01a |
| 2 | Broker | NATS account + Kafka `{tenant}.` prefixed topics & PREFIXED ACLs | MT-01c |
| 3 | State | nothing provisioned per tenant — FORCE RLS + the `app.tenant_id` GUC on shared state tables | MT-01d |
| 4 | Quota | gateway per-tenant budget entry (rate + burst + max-in-flight) | MT-01e |

**Not provisioned: the schema registry** — it is platform-global (schemas are
universal contracts; partitioning them would fork the contract, MT-01d).

## Layout

| File | Purpose |
|---|---|
| `tenantctl.sh` | the orchestrator — `onboard` / `offboard`, idempotent, dependency-ordered |
| `tenant.example.env` | one-tenant definition (the single source of truth) |
| `onboard-job.yaml` | in-cluster runner: `kanz-tenancy` ns + scoped RBAC + a per-tenant Job |

## Run

Locally (against a kubeconfig with the needed tools — `kubectl`, `jq`, and
optionally `psql`/`nsc`):

```sh
cp tenant.example.env acme.env && $EDITOR acme.env
set -a; . acme.env; set +a
./tenantctl.sh onboard      # or: offboard
```

In-cluster (no local tooling):

```sh
kubectl apply -f onboard-job.yaml          # ns + RBAC + the example Job
kubectl -n kanz-tenancy create configmap tenantctl --from-file=tenantctl.sh
kubectl -n kanz-tenancy wait --for=condition=complete job/tenant-onboard-acme
```

## Idempotency & offboarding

- Every step is **idempotent** — a partial/failed run re-runs cleanly
  (`apply`, `create --dry-run | apply`, `CREATE … IF NOT EXISTS`, ACL/topic
  `--if-not-exists`). Re-onboarding an existing tenant reconciles it.
- **Offboard reverses the order** (quota → state → broker → identity) so a
  credential is never revoked before what depends on it. ACL removal precedes
  Kafka topic deletion so no orphan grant survives.
- Tenant **state rows are retained for audit** on offboard unless `PURGE_ROWS=1`
  — deleting a client's data is a one-way door, kept opt-in. `PURGE_ROWS=1`
  only deletes `portfolios` (cascading to `positions`/`applied_keys`) in the
  single database `ADMIN_DATABASE_URL` points at — it does not reach the
  ledger, orders, or fund events, which live in other databases and are not
  purged by this step.

## Dependencies / current edges

- **NATS** account onboarding is dynamic (no reload) only with the JWT
  account-resolver + an operator (`NATS_OPERATOR`); otherwise `tenantctl` prints
  the static `nats/tenancy.yaml` append + reload step (the MT-01c static
  template). Per-account JetStream streams are bootstrapped by running the
  `nats-bootstrap` script on the tenant account.
- **Kafka** onboard/offboard each render a one-shot Job (the broker ACL writes
  need the `kafka-provisioner` SVID over the SSL listener); offboard revokes
  ACLs before deleting topics.
- **State** needs no per-tenant provisioning at all — isolation is FORCE RLS + the `app.tenant_id` GUC. On offboard, rows are kept for audit unless `PURGE_ROWS=1`, which needs `ADMIN_DATABASE_URL` and only purges `portfolios`/`positions`/`applied_keys` in that one database — the ledger, orders, and fund events (other databases) are untouched by this step

## Tenant onboarding golden path (PARITY-06e)

Severity **ticket** · rehearsed by `infra/onboarding/provision-tenant.sh`.


Provision a new institutional tenant end-to-end — across every MT-01 isolation
layer, its AUTH-01b policy bundle, per-tenant quotas, data-source binding, and a
first portfolio — then prove isolation holds before handover. The whole path is
scripted (`infra/onboarding/provision-tenant.sh`); this runbook is the operator's
guide to running and verifying it.

### One command

```sh
cd infra/onboarding
TENANT=acme ADMIN_SUBJECT=alice@acme.com ./provision-tenant.sh
```

Each step is idempotent and re-runnable (`STEP=<step>` runs one), so a partial
provision resumes rather than restarts.

### What it does, layer by layer

| Step | Layer | Action |
|---|---|---|
| storage | MT-01d | Verify Postgres RLS + FORCE RLS is active — the tenant's rows are scoped by `app.tenant_id` on the shared clusters (no per-tenant DB) |
| broker | MT-01c | Confirm envelope `tenant_id` stamping (the `bus.Producer` requires a non-empty Tenant); events share subjects, isolation is by tenant claim |
| policy | AUTH-01b | Install the tenant's role→permission bundle (the `risk-authz.json` policy-as-data shape) as a ConfigMap; map the IdP admin group → `risk.admin` |
| quota | MT-01e | Upsert the tenant into the gateway `QuotasFile` (its own rate/burst/in-flight caps, not the shared default) |
| seed | data-source | Bind feed entitlements and seed a first portfolio (`test/load/seed`) so the read path serves real data day one |
| verify | AUTH-01 | The tenant reads its own portfolio (200) **and** a cross-tenant probe is denied (403/404) — the gate that must hold before handover |

### Portfolio scoping (per-user, within a tenant)

Tenant isolation is the outer boundary; within a tenant, a principal can be
further restricted to specific portfolios via the `portfolios` claim
(`auth.ClaimPortfolios`). Grant an analyst access to a subset by populating that
claim in the IdP token mapping — the `PolicyAuthorizer` denies any portfolio not
in the allow-list even inside the correct tenant. Leave the claim empty for
tenant-wide access (the admin/reader default).

### The isolation gate is the acceptance bar

The `verify` step is not optional — a provision that leaks cross-tenant is worse
than no provision. It reproduces the MT-01g isolation contract for the new tenant:
its principal reads its own data, and a probe carrying a different tenant is
denied end-to-end at the gateway. If `verify` fails, the tenant is NOT handed
over; chase the failing layer (`STEP=policy`/`quota`/`storage` re-run) before
retrying.

### Handover checklist

- [ ] `provision-tenant.sh` completed with `isolation holds`.
- [ ] Admin subject can authenticate through the IdP and is mapped to `risk.admin`.
- [ ] First portfolio queryable via the gateway (exposure + measures return data).
- [ ] Per-tenant quota confirmed in the gateway ConfigMap.
- [ ] Onboarding recorded as evidence for SOC 2 CC6.x (see `services/audit/README.md`).
