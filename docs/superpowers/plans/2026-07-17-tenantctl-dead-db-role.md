# Retire the dead per-tenant Postgres role — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Delete `kanz_tenant_<tenant>` — a Postgres LOGIN role that nothing connects as and that isolates nothing — from `tenantctl.sh`, and make preflight demand a DB credential only on the one path that still opens a psql connection.

**Architecture:** `onboard_db()` is deleted outright (its entire body is role creation). `offboard_db()` keeps only the `PURGE_ROWS` path. Preflight's DB prerequisite narrows to `offboard` + `PURGE_ROWS=1`. The arch guard's credential assertion moves from the onboard case to a new offboard case, so coverage relocates rather than disappears.

**Spec:** `docs/superpowers/specs/2026-07-17-tenantctl-dead-db-role-design.md` (commit `080a85d`). Read it before starting: it carries the ground truth that justifies the deletion, and re-deriving it wastes a cluster you do not have.

**Tech Stack:** bash (`tenantctl.sh`), POSIX sh (`provision-tenant.sh`), Go 1.x arch tests (`test/arch/`), Postgres RLS.

## Global Constraints

- Run all Go commands from `kanz/` with `GOFLAGS=-mod=mod`.
- `gofmt -l` clean, `go vet` clean, `go test ./... -count=1` green.
- **There is no cluster, no Vault, no Postgres in this environment.** The only dev rig is a kind cluster with none of those. Every claim in this plan is verified by reading code and running the arch guards — never by connecting to a database. Do not write a verification step that needs psql.
- **Never run `os.Environ()` in these tests.** An engineer with `ADMIN_DATABASE_URL` exported would silently make preflight pass and the guard would stop proving anything. Build `cmd.Env` explicitly.
- **Always pin `KUBECONFIG` to a nonexistent path in these tests.** See `onboarding_test.go:265-278` — this was not hypothetical; an earlier version created real objects on a real cluster.
- Tenant isolation is `FORCE ROW LEVEL SECURITY` + the `app.tenant_id` GUC. Do not touch any migration or RLS policy.

---

### Task 1: Delete the role from `tenantctl.sh` and re-gate preflight

The script change and the guard change are **one task on purpose**: a reviewer cannot sensibly approve "`onboard_db` is deleted" while rejecting "preflight no longer demands the credential it used". Splitting them leaves the script in a state where it refuses an onboard over a credential for a step that no longer exists.

**Files:**
- Modify: `kanz/infra/tenancy/tenantctl.sh` (header `18-77`, preflight `114-123`, `onboard_db` `386-408`, `offboard_db` `409-424`, chain `445-449`)
- Test: `kanz/test/arch/onboarding_test.go` (modify `TestTenantctlOnboardRefusesWithoutPrerequisites` at `279-356`; add one new test after it)

**Interfaces:**
- Consumes: existing helpers `moduleRoot(t)`, `readOnboardingScript(t, root, rel)`, consts `probeTenant` (`:220`), `tenantctlRelPath` (in the `const` block at `:65`).
- Produces: `offboardProbeTenant` const, `TestTenantctlOffboardPurgeRefusesWithoutAdminDSN`. Task 2 relies on `TENANT_DB_PASSWORD` and `kanz_tenant` being absent from `kanz/` after this task.

- [ ] **Step 1: Write the failing assertions on the onboard guard**

In `onboarding_test.go`, in `TestTenantctlOnboardRefusesWithoutPrerequisites`, replace the `wantSubstrings` block (currently lines `335-346`) with the following. The `ADMIN_DATABASE_URL` entry is **removed** and replaced by a negative assertion — onboard no longer performs DB work, so naming a DB credential in its refusal would misstate the cause.

```go
	wantSubstrings := []string{
		"REFUSED: cannot onboard tenant '" + probeTenant + "'",
		"nsc on PATH + NATS_OPERATOR set",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(output, want) {
			t.Fatalf("tenantctl.sh onboard output missing %q — a clean environment (no NATS_OPERATOR, "+
				"no TENANTCTL_MANUAL_NATS escape) must name it as a missing prerequisite before "+
				"refusing. Output:\n%s", want, output)
		}
	}

	// Onboard performs NO database work: the per-tenant role this script used to
	// mint isolated nothing (every RLS policy keys only on the app.tenant_id GUC,
	// and FORCE RLS already binds the owner), and provision-tenant.sh's storage
	// step creates nothing per tenant by design. So preflight must NOT demand a
	// DB credential to onboard. Naming one here would be a refusal that lies
	// about its own cause — the ONBOARD-M3 defect inverted: that one lied about
	// success, this would lie about why it failed. Both mislead the operator.
	for _, unwanted := range []string{"ADMIN_DATABASE_URL", "TENANT_DB_PASSWORD"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("tenantctl.sh onboard refusal names %q — onboard opens no psql connection, so a "+
				"DB credential is not one of its prerequisites. The DB prereq belongs ONLY to the "+
				"offboard purge path (see TestTenantctlOffboardPurgeRefusesWithoutAdminDSN). "+
				"Output:\n%s", unwanted, output)
		}
	}
```

- [ ] **Step 2: Write the new offboard guard**

Add this after `TestTenantctlOnboardRefusesWithoutPrerequisites` ends (after line `356`). This is where the credential assertion **relocates** to — the path that actually opens a psql connection.

Note honestly what this test is: with `PURGE_ROWS=1` it is **red-on-mutation, not red-today**. Today preflight demands `ADMIN_DATABASE_URL` unconditionally, so this case already passes; its job is to hold that behavior in place through the refactor. The genuinely-red assertion is the `PURGE_ROWS` unset subtest below, which fails against the current script. Step 4 proves both.

```go
// offboardProbeTenant is a distinct probe name from probeTenant above: this
// test exercises the OFFBOARD path, and sharing a constant would make a future
// failure message imply the two guards share a run or a tenant.
const offboardProbeTenant = "tenantctl-offboard-purge-guard-probe"

// TestTenantctlOffboardPurgeRefusesWithoutAdminDSN pins where the DB credential
// prerequisite lives after the per-tenant role was retired.
//
// The role (kanz_tenant_<tenant>) was deleted because it was dead in every
// sense: nothing connected as it (services take per-service DSNs from Vault,
// authorized by SPIFFE), it isolated nothing (every RLS policy keys only on
// current_setting('app.tenant_id'); current_user appears in no policy; FORCE
// RLS already binds the owner), and the password it minted was persisted to no
// secret store. Deleting it removed a credential and no boundary.
//
// That left exactly ONE path in this script that opens a psql connection:
// offboard with PURGE_ROWS=1. This test pins both halves of the resulting rule,
// because each half fails differently and both failures harm an operator:
//
//   - PURGE_ROWS=1 without ADMIN_DATABASE_URL must REFUSE (exit 2) and name the
//     credential. Losing this means a purge silently doing nothing while
//     reporting success — a departed tenant's rows kept forever, believed gone.
//   - PURGE_ROWS unset must NOT name a DB credential. Demanding one for a run
//     that touches no database is a refusal that lies about its cause.
//
// Like the onboard guard above, this runs the real script: cmd.Env is built
// explicitly (never os.Environ(), which would let an engineer's exported
// ADMIN_DATABASE_URL make this vacuous) and KUBECONFIG is pinned to a path that
// cannot exist (see that test's doc comment — kubectl reaching a real cluster
// from a guard was not hypothetical here).
func TestTenantctlOffboardPurgeRefusesWithoutAdminDSN(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH (%v) — this guard runs tenantctl.sh for real and has no other way to "+
			"do that; install bash (git-bash on Windows, or any POSIX bash on Linux/macOS CI) to enable it", err)
	}

	root := moduleRoot(t)
	readOnboardingScript(t, root, tenantctlRelPath) // must exist and be non-empty before we bother invoking it

	run := func(t *testing.T, purgeRows string) (string, int) {
		t.Helper()
		unreachableKubeconfig := filepath.Join(t.TempDir(), "kubeconfig-does-not-exist")
		cmd := exec.Command(bashPath, tenantctlRelPath, "offboard")
		cmd.Dir = root
		env := []string{
			"PATH=" + os.Getenv("PATH"),
			"TENANT=" + offboardProbeTenant,
			"KUBECONFIG=" + unreachableKubeconfig,
			// TENANTCTL_MANUAL_NATS keeps this test focused on the DB prereq:
			// without it, a missing NATS_OPERATOR refuses first and this guard
			// would pass even if the DB rule were deleted outright.
			"TENANTCTL_MANUAL_NATS=true",
		}
		if purgeRows != "" {
			env = append(env, "PURGE_ROWS="+purgeRows)
		}
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("tenantctl.sh offboard did not even start (%v) — this proves nothing about preflight; "+
				"fix the invocation (bash %q, dir %q) rather than the script", err, tenantctlRelPath, root)
		}
		return string(out), exitErr.ExitCode()
	}

	t.Run("PURGE_ROWS=1 without ADMIN_DATABASE_URL refuses", func(t *testing.T) {
		output, code := run(t, "1")
		if code != 2 {
			t.Fatalf("tenantctl.sh offboard PURGE_ROWS=1 exited %d, want 2 (REFUSED) — PURGE_ROWS=1 is the "+
				"one path in this script that opens a psql connection, so a missing ADMIN_DATABASE_URL "+
				"(with no TENANTCTL_MANUAL_DB escape declared) must refuse before anything is touched. "+
				"Output:\n%s", code, output)
		}
		if !strings.Contains(output, "ADMIN_DATABASE_URL") {
			t.Fatalf("tenantctl.sh offboard PURGE_ROWS=1 refused without naming ADMIN_DATABASE_URL — the "+
				"operator asked for rows to be deleted and must be told exactly which credential is "+
				"missing, not merely that something is. Output:\n%s", output)
		}
	})

	t.Run("PURGE_ROWS unset does not demand a DB credential", func(t *testing.T) {
		output, _ := run(t, "")
		if strings.Contains(output, "ADMIN_DATABASE_URL") {
			t.Fatalf("tenantctl.sh offboard without PURGE_ROWS names ADMIN_DATABASE_URL — with rows kept "+
				"for audit (the default) this run opens no psql connection and has no DB prerequisite. "+
				"Demanding a credential for work that never happens is a refusal that misstates its "+
				"cause. Output:\n%s", output)
		}
	})
}
```

- [ ] **Step 3: Run the tests to verify they fail**

```bash
cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run 'TestTenantctlOnboardRefusesWithoutPrerequisites|TestTenantctlOffboardPurgeRefusesWithoutAdminDSN' -count=1 -v
```

Expected: **FAIL**, specifically —
- `TestTenantctlOnboardRefusesWithoutPrerequisites` fails on `refusal names "ADMIN_DATABASE_URL"` (preflight still demands it on onboard).
- `TestTenantctlOffboardPurgeRefusesWithoutAdminDSN/PURGE_ROWS_unset_does_not_demand_a_DB_credential` fails the same way.
- `.../PURGE_ROWS=1_without_ADMIN_DATABASE_URL_refuses` **passes already** — expected, and stated in Step 2.

If the onboard test instead fails on a `bash` skip or exit code 127, stop: the invocation is broken and nothing here is proving anything.

- [ ] **Step 4: Delete `onboard_db` and drop it from the onboard chain**

In `tenantctl.sh`, delete the whole block at lines `386-408` — the `# --- 3. state:` comment header and the entire `onboard_db()` function. The function body is *only* role creation; nothing survives it, so it goes rather than becoming an empty stub.

Then change the onboard chain (line `446`) from:

```bash
            onboard_identity; onboard_nats; onboard_kafka; onboard_db; onboard_quota
```

to:

```bash
            onboard_identity; onboard_nats; onboard_kafka; onboard_quota
```

Leave `offboard_db` in the offboard chain (line `449`) — it still purges rows.

- [ ] **Step 5: Reduce `offboard_db` to the purge path**

Replace `offboard_db()` (lines `409-424`) entirely with:

```bash
# --- 3. state: purge a departed tenant's rows (MT-01d) --------------------
# There is NOTHING per-tenant to tear down in Postgres. Tenant isolation is
# FORCE RLS + the app.tenant_id GUC (services/risk-engine/migrations/
# 0002_tenant_rls.sql), not a per-tenant role or database, so an offboard with
# PURGE_ROWS unset is a genuine no-op and says so. Rows are kept for audit by
# default; PURGE_ROWS=1 is the only path here that opens a psql connection.
offboard_db() {
  if [ "${PURGE_ROWS:-0}" != "1" ]; then
    log db "tenant rows kept for audit (set PURGE_ROWS=1 to delete them)"
    return
  fi
  if [ -z "${ADMIN_DATABASE_URL:-}" ]; then
    log db "MANUAL (TENANTCTL_MANUAL_DB=true): delete tenant '${TENANT}' rows yourself"
    MANUAL_STEPS+=("db: tenant '${TENANT}' rows not purged — DELETE FROM portfolios WHERE tenant_id = '${TENANT}'; (cascades positions/applied_keys) by hand")
    return
  fi
  log db "purging tenant '${TENANT}' rows (PURGE_ROWS=1)"
  # Scoped by an explicit WHERE, not by RLS. ADMIN_DATABASE_URL is a DB-ADMIN
  # DSN, and a superuser BYPASSES RLS even with FORCE — under which the old
  # unqualified `SET app.tenant_id; DELETE FROM portfolios;` deleted EVERY
  # tenant's portfolios. Do not "simplify" this back to relying on the GUC.
  psql "${ADMIN_DATABASE_URL}" -v ON_ERROR_STOP=1 -c \
    "DELETE FROM portfolios WHERE tenant_id = '${TENANT}';"  # cascades positions/applied_keys
}
```

**This changes the purge from RLS-scoped to WHERE-scoped — see the "Flagged finding" section at the bottom of this plan and get the lead's decision before running this step.**

- [ ] **Step 6: Re-gate the DB prerequisite in preflight**

Replace the preflight DB block (lines `114-123`) with:

```bash
  # The DB prerequisite belongs ONLY to the offboard purge path — the one place
  # left that opens a psql connection (see offboard_db). Onboard performs no DB
  # work at all: the per-tenant role that used to live here isolated nothing,
  # and provision-tenant.sh's storage step verifies RLS and creates nothing per
  # tenant. Demanding a DB-admin credential to onboard would refuse for work
  # that never happens — a refusal that misstates its own cause, which is the
  # ONBOARD-M3 lie inverted.
  if [ "${mode}" = "offboard" ] && [ "${PURGE_ROWS:-0}" = "1" ] && [ -z "${ADMIN_DATABASE_URL:-}" ]; then
    if [ "${TENANTCTL_MANUAL_DB:-false}" != "true" ]; then
      missing+=("ADMIN_DATABASE_URL (or export TENANTCTL_MANUAL_DB=true to purge the tenant's rows by hand)")
    fi
  fi
```

`TENANT_DB_PASSWORD` disappears from the script with this edit — the `elif` branch at `118-122` was its only reference.

- [ ] **Step 7: Update the script header to state the new rule**

In the header block, replace lines `39-46` with:

```bash
#   - ADMIN_DATABASE_URL: required ONLY for `offboard` with PURGE_ROWS=1 — the
#     one step that opens a psql connection — UNLESS the operator declares
#     TENANTCTL_MANUAL_DB=true (they are deleting the tenant's rows by hand).
#     Onboard needs no DB credential: there is nothing to create per tenant.
#     Tenant isolation is FORCE RLS + the app.tenant_id GUC, so a tenant's rows
#     exist the moment its services write them (provision-tenant.sh's storage
#     step VERIFIES that isolation rather than provisioning anything).
```

Then fix the precedence note at lines `74-77`, whose parenthetical still implies a DB step on onboard:

```bash
# Note on precedence: TENANTCTL_MANUAL_NATS=true (and TENANTCTL_MANUAL_DB=true)
# only ever relaxes preflight — it never forces the manual path. If nsc +
# NATS_OPERATOR (or ADMIN_DATABASE_URL, on a PURGE_ROWS=1 offboard) are actually
# present, onboard_nats / offboard_nats (and the purge step) still do the real
# thing and the flag is
```

Keep the remainder of that sentence as it is on line `78` onward.

- [ ] **Step 8: Run the tests to verify they pass**

```bash
cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run 'TestTenantctl' -count=1 -v
```

Expected: **PASS**, all subtests.

- [ ] **Step 9: Verify the script parses and the deletion is complete**

```bash
bash -n kanz/infra/tenancy/tenantctl.sh && echo "parses"
grep -rn "kanz_tenant\|TENANT_DB_PASSWORD" kanz/
```

Expected: `parses`, and the grep returns **nothing** (exit 1). Any hit in `kanz/` is an incomplete deletion. Hits under `docs/superpowers/` are expected and must be left alone — the plans and spec name the role deliberately as a historical record.

- [ ] **Step 10: Prove the new guard can actually fail**

A guard that cannot fail is the defect this repo keeps re-learning (DATA-M6 sat red for two days behind a check that skipped in CI; the verify gate printed "isolation holds" unconditionally for the life of the project). Mutate and confirm:

```bash
# Mutation: delete the DB prereq entirely from preflight (comment out the
# `missing+=(...)` line added in Step 6), then:
cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run 'TestTenantctlOffboardPurgeRefusesWithoutAdminDSN' -count=1 -v
```

Expected: **FAIL** on `PURGE_ROWS=1 without ADMIN_DATABASE_URL refuses`, with the exit-code-2 message. If it passes, the guard is vacuous — fix it before continuing.

Revert the mutation and re-run to confirm green.

- [ ] **Step 11: Run the full suite**

```bash
cd kanz && gofmt -l . && GOFLAGS=-mod=mod go vet ./... && GOFLAGS=-mod=mod go test ./... -count=1
```

Expected: `gofmt -l` prints nothing, vet silent, all packages `ok`/`no test files`, zero `FAIL`.

- [ ] **Step 12: Commit**

```bash
git add kanz/infra/tenancy/tenantctl.sh kanz/test/arch/onboarding_test.go
git commit -m "$(cat <<'EOF'
fix(tenancy): delete the dead per-tenant Postgres role

kanz_tenant_<tenant> was dead on every axis. Nothing connected as it: services
take per-service DSNs from Vault, authorized by SPIFFE, and no DSN anywhere
named it. It isolated nothing: every RLS policy keys only on
current_setting('app.tenant_id'), current_user appears in no policy, and FORCE
RLS already binds the table owner. It granted only risk-engine's three tables,
and the LOGIN password it minted was persisted to no secret store.

onboard_db's whole body was that role, so it is deleted rather than emptied --
onboard now opens no psql connection, which is what provision-tenant.sh's
storage step already documented as correct. The DB prerequisite follows the
work to the only path that still connects: offboard with PURGE_ROWS=1.
TENANT_DB_PASSWORD is now unreferenced and gone.

The arch guard's credential assertion moves with it: the onboard case now
asserts a DB credential is NOT named (demanding one for work that never happens
is the ONBOARD-M3 lie inverted), and a new offboard case pins the refusal on
the purge path.

This removes a credential and no boundary.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Correct the docs that state the dead behavior

Stale docs are how a deleted concept comes back: the next engineer reads `tenant.example.env`, sets `TENANT_DB_PASSWORD`, and files a bug when nothing uses it.

**Files:**
- Modify: `kanz/infra/tenancy/tenant.example.env:12-15`
- Modify: `kanz/infra/tenancy/README.md:72`
- Modify: `kanz/infra/onboarding/provision-tenant.sh:23`
- Modify: `kanz/infra/tenancy/onboard-job.yaml:72`
- Verify (likely no change): `kanz/docs/runbooks/tenant-onboarding.md:29`

**Interfaces:**
- Consumes: Task 1's completed deletion. Do not start this task until `grep -rn "kanz_tenant\|TENANT_DB_PASSWORD" kanz/` is clean.
- Produces: nothing downstream.

- [ ] **Step 1: Fix `tenant.example.env`**

Replace lines `12-15`:

```sh
# State (MT-01d). Nothing is provisioned per tenant: isolation is FORCE RLS +
# the app.tenant_id GUC, so a tenant's rows exist as soon as its services write
# them. ADMIN_DATABASE_URL is a DB-admin DSN needed ONLY to purge a departed
# tenant's rows on offboard (PURGE_ROWS=1).
# ADMIN_DATABASE_URL=postgres://admin@db:5432/risk
# PURGE_ROWS=1
```

- [ ] **Step 2: Fix `tenancy/README.md`**

Replace line `72`:

```markdown
- **State** needs no per-tenant provisioning at all — isolation is FORCE RLS + the `app.tenant_id` GUC. On offboard, rows are kept for audit unless `PURGE_ROWS=1`, which needs `ADMIN_DATABASE_URL`
```

- [ ] **Step 3: Fix the `provision-tenant.sh` header**

Replace lines `20-25` (the `Requires:` block):

```sh
# Requires: kubectl, psql (or the cnpg plugin), and the target cluster context —
# plus, for the tenantctl.sh `infra` step this script composes: nsc + NATS_OPERATOR
# (else the NATS account step logs a manual instruction and returns 0 instead of
# provisioning anything), and jq (the gateway quota step reads/writes the quota
# ConfigMap through it). No DB credential is needed to onboard: nothing is
# provisioned per tenant in Postgres (see step 1 below).
```

Do **not** touch the storage step at `138-143` — it is already correct and is the evidence this whole change rests on.

- [ ] **Step 4: Fix `onboard-job.yaml`**

Replace line `72`:

```yaml
          # The nsc step no-ops unless NATS_OPERATOR is set. Onboard does no DB
          # work: state isolation is FORCE RLS + the app.tenant_id GUC, with
          # nothing to provision per tenant.
```

- [ ] **Step 5: Verify the runbook**

Read `kanz/docs/runbooks/tenant-onboarding.md:29`. It describes the storage step as verifying RLS + FORCE RLS with "no per-tenant DB" — which is now *more* true, not less. Confirm it reads correctly and leave it alone if so. Do not edit it just to have edited it.

- [ ] **Step 6: Verify nothing references the dead behavior**

```bash
grep -rni "per-tenant.*role\|login role\|TENANT_DB_PASSWORD\|kanz_tenant" kanz/
sh -n kanz/infra/onboarding/provision-tenant.sh && echo "parses"
cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -count=1
```

Expected: grep returns nothing, `parses`, arch tests green. `test/arch/` includes static guards that read these scripts as text, so a doc edit that breaks one is caught here.

- [ ] **Step 7: Commit**

```bash
git add kanz/infra/tenancy/tenant.example.env kanz/infra/tenancy/README.md kanz/infra/onboarding/provision-tenant.sh kanz/infra/tenancy/onboard-job.yaml
git commit -m "$(cat <<'EOF'
docs(tenancy): stop documenting the deleted per-tenant DB role

The role is gone; these four files still told operators to set
TENANT_DB_PASSWORD and described state provisioning as something that happens
per tenant. Nothing is provisioned per tenant: isolation is FORCE RLS + the
app.tenant_id GUC. ADMIN_DATABASE_URL survives for exactly one job -- purging a
departed tenant's rows on a PURGE_ROWS=1 offboard.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Flagged finding — needs the lead's decision before Task 1 Step 5

**The existing purge may delete every tenant's rows.** `tenantctl.sh:417-418` today is:

```bash
psql "${ADMIN_DATABASE_URL}" -v ON_ERROR_STOP=1 -c \
  "SET app.tenant_id = '${TENANT}'; DELETE FROM portfolios;"  # cascades positions/keys
```

That `DELETE` has **no `WHERE` clause**. It is scoped entirely by RLS reading the GUC. But `ADMIN_DATABASE_URL` is documented as a DB-admin DSN, and **a superuser bypasses RLS even with `FORCE`** — the repo states this itself in three places (`0002_tenant_rls.sql:12`, `kanz-migrate/main.go:19`, `secretproviderclass.yaml:355-357`). If that DSN belongs to a superuser, `PURGE_ROWS=1` deletes **all tenants' portfolios**, cascading to positions and applied_keys.

I cannot confirm which role that DSN carries — there is no Vault and no Postgres in this environment, and the repo does not pin it. So this is a real risk, not a confirmed bug.

Task 1 Step 5 as written fixes it by scoping the `DELETE` with an explicit `WHERE tenant_id = '${TENANT}'`, which is correct whether or not the DSN is a superuser. It is one line and strictly safer.

This is **wider than the "role only" cut you approved**, and it changes the behavior of code the spec's non-goals said to keep. Three options:

1. **Take the `WHERE` fix** (what Step 5 currently does). One line; removes the risk permanently; keeps me from knowingly preserving an unqualified `DELETE` I just flagged.
2. **Preserve the current SQL verbatim** and file the purge risk as its own task. Keeps this change a pure deletion, but ships a known-suspect `DELETE`.
3. **Verify the DSN's role first** — needs a real Postgres, which this environment does not have. Defers the work.

I recommend **(1)**.
