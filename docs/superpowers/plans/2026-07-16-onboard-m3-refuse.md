# ONBOARD-M3 — tenant onboarding must not report success without doing the work

## Context

`kanz/infra/tenancy/tenantctl.sh onboard` prints `>> [done] tenant X onboarded` while having
provisioned **nothing**. Verified in the code, three separate ways:

1. **`onboard_nats`** — without `nsc` on PATH **and** `NATS_OPERATOR` set, it logs
   `static mode: add account '<T>' to nats/tenancy.yaml … then reload` and **returns 0**.
2. **`onboard_db`** — without `ADMIN_DATABASE_URL`, it logs `skip (set ADMIN_DATABASE_URL …)` and
   **returns 0**.
3. **`onboard_kafka`** — applies a Kubernetes Job and **never waits for it** (there is no
   `kubectl wait` anywhere in the file), so it returns 0 regardless of whether the Job created the
   topics and ACLs, or failed.

The entrypoint chains them (`onboard_identity; onboard_nats; onboard_kafka; onboard_db;
onboard_quota`) and then unconditionally logs `tenant ${TENANT} onboarded`.

**Why this is severe rather than untidy.** Under SEC-M3 the production broker runs
`tls { verify: true, verify_and_map: true }`: a tenant with no NATS account authenticates into **no
account** and cannot publish or subscribe a single event. And since ONBOARD-M1, the client golden
path (`provision-tenant.sh`) *composes* this script — so it proceeds to install policy, seed a
portfolio and hand the tenant to a client, on top of infrastructure that does not exist.

**It breaks the platform's own oldest rules,** in the one script that onboards a paying client:
*"'Nothing to check' and 'checked, and fine' must never be the same observable event"* and *"no
simulator reachable by omission — absence of configuration means degrade or REFUSE, never
fabricate"*. A missing `nsc` is absence of configuration. Printing "onboarded" is fabricating a
tenant.

## Ground truth — verified by the controller, do not re-derive

- The three defects above are read from the code, not inferred.
- **Static mode is a legitimate workflow, not a bug in itself.** `infra/nats/tenancy.yaml`'s own
  header documents the manual path: *"Add a tenant by appending an account + user keyed on the tenant
  workload's SPIFFE URI SAN, then `nats-server --signal reload`"*. The defect is that it is
  **silent** and the script then **claims success**. Do not delete static mode — make it honest.
- `offboard_db` has the same silent skip (`[ -z "${ADMIN_DATABASE_URL:-}" ] && { log db "skip"; return; }`),
  and `offboard_nats` the same static-mode branch. The fix must be symmetric or offboard grows the
  same lie.
- `TENANT_DB_PASSWORD` is enforced only as `${TENANT_DB_PASSWORD:?…}` *inside the SQL heredoc*, i.e.
  after `psql` has already been invoked. A preflight must catch it earlier.
- `nsc add account … || true` swallows failures, then `nsc edit`/`nsc push` run regardless.
- There is **no Kubernetes cluster on this box**, so the script cannot be run end-to-end. This is
  precisely why the fix is a **preflight**: refusal happens before the first `kubectl`, which makes
  it executable and testable here.

## Global Constraints

- **Do not** change `provision-tenant.sh` (the golden path). It composes this script and inherits the
  fix through the exit status. ONBOARD-M1's guard covers it and must stay green.
- **Do not** delete static mode or the manual DB path. They are real workflows; they must become
  explicit and must never report success.
- Shell only in Task 1; Go only in Task 2.
- `test/arch` stays green — including ONBOARD-M1's `onboarding_test.go`.

## Task 1 — Refuse, or say PARTIAL. Never lie.

**File:** `kanz/infra/tenancy/tenantctl.sh`

**Requirements:**

1. **A preflight that runs before any mutation**, for both `onboard` and `offboard`. It collects
   *every* missing prerequisite and refuses **once**, naming all of them — not one per run, which
   turns provisioning into a guessing game. Refusal is a non-zero exit and must happen before the
   first `kubectl`/`psql`/`nsc` call.
2. **Prerequisites to check:** `kubectl` and `jq` on PATH; for the NATS step, `nsc` on PATH **and**
   `NATS_OPERATOR` set; for the DB step, `ADMIN_DATABASE_URL` **and** `TENANT_DB_PASSWORD`
   (the latter only matters when the DB step will actually run).
3. **The affirmative escape, per the `DATAMASTER_ALLOW_SIM` stance:** `TENANTCTL_MANUAL_NATS=true`
   and `TENANTCTL_MANUAL_DB=true` let an operator declare they are doing that step by hand. Absent
   the flag, a missing prerequisite is a REFUSAL — never a skip. The flag is the only way to proceed
   without the tooling, and it must be an explicit, affirmative choice.
4. **A manual step means the tenant is NOT onboarded.** When any step was manual, the script must
   **not** print `tenant X onboarded`. It prints a PARTIAL summary listing exactly what a human must
   still do, and **exits non-zero** — so the composing golden path (`set -eu`) aborts rather than
   seeding a portfolio onto infrastructure that does not exist. Use a distinct exit code (e.g. 3 for
   PARTIAL, 2 for REFUSED) and document both in the header.
5. **`onboard_kafka` must wait for its Job** and fail if the Job fails — `kubectl wait
   --for=condition=complete` with a bounded timeout, and a failure must surface (consider also
   waiting on `condition=failed` so a failed Job fails fast rather than burning the timeout). A
   fire-and-forget apply is the same lie as a silent skip.
6. **`nsc add account … || true`**: the `|| true` swallows a real failure and lets `edit`/`push` run
   against an account that may not exist. Make it honest — either tolerate only the
   already-exists case, or drop the swallow. Say which you chose and why.
7. Symmetric treatment for `offboard_db` / `offboard_nats`.
8. Update the header comment: the exit codes, the two `TENANTCTL_MANUAL_*` flags, and the rule that
   a manual step never reports success.

**Verification (must be EXECUTED — no cluster, so drive the preflight, which is the point):**
- `bash -n infra/tenancy/tenantctl.sh` → parses.
- **REFUSAL:** `TENANT=probe bash infra/tenancy/tenantctl.sh onboard` with `NATS_OPERATOR` and
  `ADMIN_DATABASE_URL` unset → **non-zero exit**, output names the missing prerequisites, and
  **does not** contain `onboarded`. Paste it verbatim.
- **PARTIAL:** same with `TENANTCTL_MANUAL_NATS=true TENANTCTL_MANUAL_DB=true` → exits non-zero with
  a PARTIAL summary naming the manual steps, and **does not** contain `tenant probe onboarded`.
  (It may fail at the first `kubectl` if no cluster — if so, that is acceptable ONLY if the PARTIAL
  refusal happens first; report exactly what you observed rather than what you expected.)
- The refusal must not touch the cluster: confirm no `kubectl`/`psql`/`nsc` ran before it (reason
  from the code and say so).

## Task 2 — Prove the refusal, executably

**File:** `kanz/test/arch/onboarding_test.go` (extend — do not create a second file).

**Why:** ONBOARD-M1's guard is static: it reads the scripts as text. This one **runs** `tenantctl.sh`
and asserts it refuses. The whole ONBOARD epic exists because nobody ever ran these scripts together;
a static guard cannot see a script that lies at runtime.

**Requirements:**

1. A test that executes `bash infra/tenancy/tenantctl.sh onboard` with `TENANT` set to a probe value
   and **a deliberately empty environment** (no `NATS_OPERATOR`, no `ADMIN_DATABASE_URL`), and
   asserts: non-zero exit, stderr/stdout names the missing prerequisites, and the output **never
   contains `onboarded`**. The last assertion is the load-bearing one — it is the exact lie M3 fixes.
2. It must **not** require or touch a cluster. If the preflight is correct, nothing reaches
   `kubectl`. Do not skip when `kubectl` is absent — that would make the test vacuous on CI.
3. Skip cleanly **only** if `bash` is unavailable, with a stated reason.
4. Non-vacuous: assert the script was actually executed (a non-zero exit for the *wrong* reason —
   e.g. "no such file" — must fail the test, not pass it).
5. Reuse `moduleRoot(t)`. House style: the doc comment explains WHY, failure messages say what to DO.

**Verification (must be EXECUTED):**
- Passes on the tree after Task 1.
- **Mutation:** make the preflight return 0 unconditionally (or restore a silent skip in
  `onboard_nats`) → the test FAILS. Restore with a targeted edit (never `git checkout -- .`), confirm
  green.
- `go test ./test/arch/... -count=1` green; `gofmt -l .` clean; `git status --short` shows no
  modified tracked files when finished.
