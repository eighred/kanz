# ONBOARD-M4 — the RLS check cannot detect RLS being off

## Context

`provision-tenant.sh` step 1 claims to verify the platform's core isolation guarantee before a tenant
is provisioned:

```sh
echo "-- [1/5] verify RLS isolation is active (kanz-risk, kanz-books)"
for c in kanz-risk kanz-books; do
  k -n "$DATA_NS" exec "$c-1" -- psql -tAc \
    "select relname from pg_class where relrowsecurity and relforcerowsecurity limit 1" \
    >/dev/null || { echo "FATAL: FORCE RLS not active on $c — MT-01d not deployed"; exit 1; }
done
```

**It is vacuous.** `>/dev/null` discards the query output — the only thing carrying the answer — and
the branch tests **exit status alone**. `psql -tAc` exits **0** for a successful query that returns
**zero rows**. So:

| state | psql exit | script concludes |
|---|---|---|
| RLS **active** | 0 (one relname) | pass ✓ *(right, by accident)* |
| **RLS OFF** | **0 (empty)** | **pass** ✗ — the control is absent and it says nothing |
| namespace/pod missing, psql unreachable | non-zero | *"FORCE RLS not active — MT-01d not deployed"* ✗ |

**It passes precisely when the security control is missing, and fails claiming the control is
missing when the infrastructure is.** The `|| { … }` never fires for the condition it names.

Observed live 2026-07-16, first run of the golden path against a real cluster: the real failure was
`Error from server (NotFound): namespaces "kanz-data" not found` and the script reported that FORCE
RLS was not active — **a control it never checked.**

This is the fifth instance of one family in this onboarding path, after ONBOARD-M1 (a false broker
claim), M2 (a gate that tested nothing), and M3 (steps that returned 0 having done nothing): **a
check reporting a conclusion it did not reach.** It is why `app_current_tenant()` RAISES instead of
returning zero rows — *"a missing tenant and a tenant with no data are both zero rows, and the
dangerous one is silent"* (`KANZ_BRAIN.md`). This check made exactly that mistake about RLS itself.

## Ground truth — verified by the controller, do not re-derive

- The `>/dev/null` and the exit-status-only branch are read from the code, not inferred.
- Live behaviour, `TENANT=acme … sh ./provision-tenant.sh` against `kind-kanz-dryrun`: exit **1**,
  output exactly `Error from server (NotFound): namespaces "kanz-data" not found` followed by
  `FATAL: FORCE RLS not active on kanz-risk — MT-01d not deployed`. It mutated nothing.
- `kanz-data`, `kanz-risk` and `kanz-books` **do not exist** on that cluster, and there is no `psql`
  on this box — so the RLS-**off** path cannot be exercised here. The **cannot-run** path can, and
  must be.
- The script is `#!/bin/sh` with `set -eu`. Command substitution inherits the child's status, so
  `out="$(k … )" || { … }` captures the failure branch cleanly.
- `DATA_NS` defaults to `kanz-data`; `STEP=storage` runs step 1 alone, which is what makes this
  testable here.

## Global Constraints

- **Do not** change `tenantctl.sh` or the verify step. M2/M3 just landed there and are reviewed.
- **Do not** weaken step 1 into a skip. It must fail closed. "Nothing to check" and "checked, and
  fine" must never be the same observable event.
- Shell only in Task 1; Go only in Task 2. `#!/bin/sh` — **no bashisms**.
- `test/arch` stays green — all five onboarding guards.

## Task 1 — Make step 1 detect what it claims to

**File:** `kanz/infra/onboarding/provision-tenant.sh` (step 1 and its header note).

**Requirements:**

1. **Capture the output; stop discarding it.** The query returns a relname when FORCE RLS is on and
   nothing when it is off. That output *is* the check — `>/dev/null` threw the answer away.
2. **Assert the result is non-empty.** An empty result means **RLS is OFF** ⇒ FATAL, naming the
   cluster, and saying MT-01d is not deployed — which is what the current message *claims* and never
   proves.
3. **Distinguish the two failures, and this is the substance:**
   - **Could not run the check** (kubectl/exec/psql failed — namespace absent, pod missing, DB
     unreachable): a distinct FATAL saying the check could **not be run** and naming the underlying
     error. It must **not** say RLS is inactive — it does not know that.
   - **Ran, and RLS is off:** the existing FATAL, now actually earned.
4. Keep the `for c in kanz-risk kanz-books` loop and fail on the first bad cluster.
5. Note in the header (or beside the step) *why* the output is captured rather than discarded — a
   future reader "tidying" a `>/dev/null` back in would silently restore a vacuous check. This is the
   same reasoning `app_current_tenant()` documents: an empty result and a broken query must never be
   the same observable event.

**Verification (must be EXECUTED — a real cluster is reachable; the RLS-off path is not testable
here, the cannot-run path is):**
- `sh -n infra/onboarding/provision-tenant.sh` → parses.
- **The live misdiagnosis is gone.** From `kanz/infra/onboarding`, with the real cluster context:
  `TENANT=probe STEP=storage sh ./provision-tenant.sh` → exits non-zero, says the check could **not
  be run** and names the `kanz-data` NotFound, and the output does **NOT** contain
  `FORCE RLS not active`. Paste it verbatim.
- **It must not mutate the cluster:** confirm `kubectl --context kind-kanz-dryrun get ns tenant-probe`
  is still NotFound afterwards. (Step 1 is read-only by design — prove it stayed so.)
- `go test ./test/arch/... -count=1` green.

## Task 2 — Pin the distinction

**File:** `kanz/test/arch/onboarding_test.go` (extend; five guards live there).

**Why:** the vacuity survived because nothing ran step 1. A static guard cannot see that `psql` exits
0 on an empty result — that is a runtime fact about a program this repo does not own.

**Requirements:**

1. Execute `sh infra/onboarding/provision-tenant.sh` with `TENANT` a probe value, `STEP=storage`, an
   explicit env, and **`KUBECONFIG` pinned to a nonexistent path** so `kubectl` cannot run. Assert:
   non-zero exit, the output says the check could not be run, and it **does NOT** contain
   `FORCE RLS not active` — the misdiagnosis, pinned dead.
2. Do **not** inherit the developer's env (`exec.Command`'s `Env`).
3. **A real `kind` cluster is reachable from this box and it is the user's** — the KUBECONFIG pin is
   what keeps this test off it. An earlier agent mutated that cluster by accident because bash
   recomputes `HOME` regardless of the env passed. Do not repeat it.
4. Non-vacuous: assert a specific exit code and that the script actually ran (a 127 must fail, not
   pass).
5. Reuse `moduleRoot(t)`; follow the file's existing style.

**Verification (must be EXECUTED):**
- Passes after Task 1.
- **Mutation:** restore `>/dev/null` + the single `||` branch (the original vacuous form) → the test
  FAILS. Restore with a targeted edit (never `git checkout -- .`), confirm green.
- `go test ./test/arch/... -count=1` green; `gofmt -l .` clean; `git status --short` shows no
  modified tracked files under `kanz/`.
