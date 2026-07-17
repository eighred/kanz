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
| 3 | State | Postgres per-tenant login role granted on the RLS-scoped state tables | MT-01d |
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
  — deleting a client's data is a one-way door, kept opt-in.

## Dependencies / current edges

- **NATS** account onboarding is dynamic (no reload) only with the JWT
  account-resolver + an operator (`NATS_OPERATOR`); otherwise `tenantctl` prints
  the static `nats/tenancy.yaml` append + reload step (the MT-01c static
  template). Per-account JetStream streams are bootstrapped by running the
  `nats-bootstrap` script on the tenant account.
- **Kafka** onboard/offboard each render a one-shot Job (the broker ACL writes
  need the `kafka-provisioner` SVID over the SSL listener); offboard revokes
  ACLs before deleting topics.
- **State** needs no per-tenant provisioning at all — isolation is FORCE RLS + the `app.tenant_id` GUC. On offboard, rows are kept for audit unless `PURGE_ROWS=1`, which needs `ADMIN_DATABASE_URL`
