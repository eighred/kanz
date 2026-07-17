# Retire the dead per-tenant Postgres role (`kanz_tenant_<tenant>`)

**Date:** 2026-07-17
**Scope:** `kanz/infra/tenancy/tenantctl.sh`, `kanz/test/arch/onboarding_test.go`, and the docs that state the dead behavior.
**Lead decision (2026-07-17):** delete the role. Do not wire it up.

## The defect

`tenantctl.sh` `onboard_db()` mints a Postgres `LOGIN` role `kanz_tenant_${TENANT}` and grants it
`SELECT, INSERT, UPDATE, DELETE` on `portfolios, positions, applied_keys`. The role is dead on
every axis that could justify it. This was noted as a real defect by the MT-02 plan
(`docs/superpowers/plans/2026-07-17-mt-02-per-tenant-compute.md:52`), declared out of scope, and
recorded there as "filed separately" — **it was never filed**; no such task exists in
`KANZ_TASKS.md`. This spec is that work.

### Ground truth (verified against code, not roadmap)

1. **Nothing ever connects as it.** Every service takes its DSN from Vault via a per-service
   `SecretProviderClass` (`infra/security/secrets/secretproviderclass.yaml`), each exposing `dsn`
   (the non-superuser app role) and `migrate_dsn` (the schema-owning role), authorized by SPIFFE
   identity. No Vault path, no `SecretProviderClass`, and no DSN anywhere references
   `kanz_tenant_*`.
2. **It isolates nothing.** Every RLS policy in the repo keys *only* on
   `current_setting('app.tenant_id', true)` — e.g. `services/risk-engine/migrations/0002_tenant_rls.sql:60-64`.
   `current_user` appears in **no** policy. `FORCE ROW LEVEL SECURITY` already applies the policy to
   the table owner, so the role adds no boundary that does not already exist. This matches MT-02
   ground truth #4, independently re-verified here.
3. **It is arbitrary even on its own terms.** It grants only risk-engine's three tables. It grants
   nothing on `orders`, `ledger_entries`, or `fund_events`.
4. **It mints a credential stored nowhere.** `CREATE ROLE ... LOGIN PASSWORD '${TENANT_DB_PASSWORD}'`
   — the password is persisted to no secret store, so the credential is unusable *and* unrotatable.
5. **The golden path already contradicts it.** `provision-tenant.sh:138-143` documents the storage
   step as *"no per-tenant database, one policy-enforced table. Nothing to create per tenant... This
   step is a VERIFY, not a mutate"*, then its `infra` step delegates to `tenantctl.sh`, which
   mutates anyway. One script's storage step disproves the other's.

**Security direction:** this removes a credential and **no** boundary. `FORCE RLS` + the
`app.tenant_id` GUC is the entire tenant boundary and always was.

### Explicitly rejected: wiring the role up

Making the role real would require a per-tenant Vault path, a per-tenant `SecretProviderClass`,
per-tenant rotation, and re-keying every RLS policy onto `current_user`. That is a new subsystem
that contradicts the per-service DSN model, to defend a threat (a compromised service process
forging its own GUC) that no current policy addresses. Out of scope; not a prerequisite for this
deletion.

## The change

### 1. `onboard_db()` — delete entirely

`tenantctl.sh:393-408`. The function body is *only* role creation; removing the role leaves nothing,
so the function goes rather than becoming an empty stub. Remove `onboard_db` from the onboard chain
(`tenantctl.sh:446`). After this, **onboard touches Postgres zero times** — which is what
`provision-tenant.sh`'s storage step already verified to be correct.

### 2. `offboard_db()` — keep only the `PURGE_ROWS` path

`tenantctl.sh:409-424`. Drop the `REVOKE` / `DROP ROLE` heredoc (lines 420-423). Keep the
`PURGE_ROWS=1` block: purging a departed tenant's rows is real functionality with no other home.
Retitle the log line, which currently announces "revoking role".

Default behavior (`PURGE_ROWS` unset) is now a genuine no-op: rows are kept for audit, as the
existing log line already promises. The manual-escape branch must move inside the `PURGE_ROWS=1`
path — otherwise an offboard that purges nothing would report PARTIAL (exit 3) for work it never
needed to do.

#### Scope the purge with an explicit `WHERE` (lead decision, 2026-07-17)

The surviving `DELETE` is today unqualified and scoped **only by RLS**:

```sql
SET app.tenant_id = '${TENANT}'; DELETE FROM portfolios;
```

`ADMIN_DATABASE_URL` is documented as a DB-admin DSN, and **a superuser bypasses RLS even with
`FORCE`** — the repo asserts this in three places (`0002_tenant_rls.sql:12`,
`cmd/kanz-migrate/main.go:19`, `secretproviderclass.yaml:355-357`). If that DSN carries a superuser,
`PURGE_ROWS=1` deletes **every tenant's** portfolios, cascading to positions and applied_keys.

Which role that DSN carries cannot be confirmed here — there is no Vault and no Postgres in this
environment, and the repo pins it nowhere. It is a real risk, not a confirmed bug. The purge is
therefore scoped by an explicit `WHERE tenant_id = '${TENANT}'`, which is correct whether or not the
DSN is a superuser and does not depend on a fact we cannot check. This is a deliberate, approved
widening of the "role only" cut: preserving an unqualified `DELETE` that this same change flagged
would be knowingly shipping the risk.

### 3. `preflight()` — gate the DB prereq on the path that opens a connection

Forced by #1. Today `tenantctl.sh:114-117` requires `ADMIN_DATABASE_URL` on **every** run, and
lines 118-122 require `TENANT_DB_PASSWORD` on onboard. Once onboard performs no DB work, refusing an
onboard for a missing DB-admin credential is a refusal that misstates its own cause — the ONBOARD-M3
defect inverted (that one lied about success; this would lie about failure). Both are preflight
telling the operator something untrue.

New rule: the DB prerequisite applies only when `mode = offboard` **and** `PURGE_ROWS=1` — the only
surviving path that opens a psql connection.

- `TENANTCTL_MANUAL_DB=true` survives as the escape, meaning narrowed to *"I will purge the rows by
  hand."* It keeps the manual-escape symmetry with `TENANTCTL_MANUAL_NATS` and keeps `finish()`'s
  PARTIAL reporting honest via `MANUAL_STEPS`.
- `TENANT_DB_PASSWORD` retires — it becomes genuinely unreferenced. Its preflight branch
  (lines 118-122) goes with it.

### 4. `onboarding_test.go` — move the coverage, keep the teeth

`TestTenantctlOnboardRefusesWithoutPrerequisites` asserts at line 338 that a clean-environment
onboard refusal names `ADMIN_DATABASE_URL`. That assertion becomes false **by design** and must be
removed from the onboard case.

What is preserved without modification:

- The load-bearing assertion — `"onboarded"` never appears (line 351). Untouched.
- The refusal itself: a missing `NATS_OPERATOR` still forces exit 2, so the guard still fails a
  neutered preflight.

What is added, so the removed coverage is relocated rather than dropped: a case pinning that
**offboard with `PURGE_ROWS=1` in a clean environment refuses (exit 2) and names
`ADMIN_DATABASE_URL`**. This is the credential-gating assertion, moved to the path that actually
needs the credential. Follow the existing test's environment discipline exactly: build `cmd.Env`
explicitly (never `os.Environ()`), and pin `KUBECONFIG` to a nonexistent path for the reason the
existing doc comment gives at lines 265-278 — that was not hypothetical.

### 5. Docs stating the dead behavior

- `kanz/infra/tenancy/tenant.example.env:12-15` — the `TENANT_DB_PASSWORD` / `ADMIN_DATABASE_URL`
  block, which describes seeding "the per-tenant login role".
- `kanz/infra/tenancy/README.md:72` — "State role provisioning is skipped unless
  `ADMIN_DATABASE_URL` is set".
- `kanz/infra/onboarding/provision-tenant.sh:23` — the header naming
  `ADMIN_DATABASE_URL + TENANT_DB_PASSWORD` as prerequisites.
- `kanz/infra/tenancy/onboard-job.yaml:72` — "psql/nsc steps no-op unless ADMIN_DATABASE_URL ...".
- `kanz/docs/runbooks/tenant-onboarding.md` — verify the storage row (line 29) still reads true; it
  describes RLS verification and should need no change, but confirm rather than assume.
- `tenantctl.sh:18-76` — the preflight header comment block describes the DB rule at length
  (lines 39-48, 74-76) and must state the new rule.

## Non-goals

- Do not re-key any RLS policy. Do not touch any migration.
- Do not change `provision-tenant.sh`'s storage step. It is already correct and is the evidence for
  this change.
- Do not remove `PURGE_ROWS`, `ADMIN_DATABASE_URL`, or `TENANTCTL_MANUAL_DB`. (Scoping the purge's
  `DELETE` with an explicit `WHERE` is approved and in scope — see above.)
- Do not touch the `__system__` tenant or per-service DSN wiring.

## Verification

- `bash -n infra/tenancy/tenantctl.sh` → parses.
- `sh -n infra/onboarding/provision-tenant.sh` → parses.
- `grep -rn "kanz_tenant\|TENANT_DB_PASSWORD" kanz/` → **zero matches**. This is the completeness
  check for the deletion. (Scoped to `kanz/`: the historical `docs/superpowers/plans/` records and
  this spec name the role deliberately and must not be rewritten.)
- `go test ./test/arch/ -run TestTenantctl -count=1` → green, from `kanz/` with `GOFLAGS=-mod=mod`.
- `go test ./... -count=1` → green; `gofmt -l` and `go vet` clean.
- **Mutation check (the guard must be non-vacuous):** restore the `ADMIN_DATABASE_URL` requirement
  to the onboard path and confirm the new offboard case still passes while nothing else regresses;
  then remove the DB prereq entirely and confirm the new offboard case **fails**. A guard that
  cannot fail is the defect this repo keeps re-learning (DATA-M6, the pre-Task-1 verify gate).

## Success criteria

`tenantctl.sh` no longer creates, grants, revokes, or drops a Postgres role; onboard opens no psql
connection; `TENANT_DB_PASSWORD` does not exist in the repo; the arch guard still refuses a clean
onboard and now also refuses a clean purging offboard; and `tenantctl.sh` and `provision-tenant.sh`
tell the same story about what tenant storage provisioning requires — nothing.
