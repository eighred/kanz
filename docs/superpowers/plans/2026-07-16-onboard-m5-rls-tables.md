# ONBOARD-M5 — assert RLS on the tables that matter, in the database that has them

## Context

ONBOARD-M4 made step 1's RLS check *able to fail*. It still checks the wrong thing, in what is
probably the wrong database. Both are pre-existing in a query M4 deliberately did not touch.

```sh
psql -tAc "select relname from pg_class where relrowsecurity and relforcerowsecurity limit 1"
```

**(1) `limit 1` proves RLS is on SOMEWHERE, not on the tenant-scoped tables.** It returns the first
relation with FORCE RLS anywhere in the database. If `positions` alone lost RLS, `portfolios` still
satisfies `limit 1` and the gate reports isolation active — while a tenant's positions read across
tenants. Same family as M1–M4: **a check reporting a conclusion broader than what it measured.**

**(2) It names no database, and the platform's own manifests say that is wrong.** `psql -tAc` with no
`-d`, run as the pod's default user, connects to `postgres`. **`infra/dr/postgres/cluster.yaml` has no
`bootstrap`/`initdb` block** for `kanz-risk` or `kanz-books` — verified — so CNPG's default applies and
the tenant tables live in database **`app`**. `pg_class` is per-database. **A healthy cluster therefore
returns empty and step 1 FATALs "MT-01d not deployed".** It fails CLOSED with a wrong reason, so it
cannot leak a tenant — but it will fire on the first real provision and send an operator hunting a
security defect that does not exist.

## Ground truth — verified by the controller, do not re-derive

- The FORCE-RLS tables, read from the migrations (note: the SQL is `FORCE  ROW LEVEL SECURITY` with
  **two spaces** — a single-space grep finds nothing):

  | cluster | service | tables that FORCE RLS |
  |---|---|---|
  | `kanz-risk` | risk-engine | `portfolios`, `positions`, `applied_keys` |
  | `kanz-books` | accounting | `ledger_entries`, `ledger_snapshots` |

  The cluster↔service mapping is by name and by table content; the DSNs live in sealed secrets and
  cannot be read here. Other services also FORCE RLS (oms, tv-sync, datamaster, wealth, alternatives,
  venue-*), but step 1 checks only these two clusters — **widening that is NOT in scope**, it is a
  separate question about which clusters exist.
- `infra/dr/postgres/cluster.yaml` contains no `bootstrap:` for either cluster ⇒ CNPG default
  ⇒ database `app`, owner `app`.
- Step 1 is read-only and, post-M4, already splits "could not run the check" from "the check failed".
  **That split must survive** — it is M4's whole contribution.
- No `psql`, no `kanz-data`, no CNPG on this box: **the RLS-verdict paths cannot be executed here.**
  The could-not-run path can, and the arch guard can.

## Global Constraints

- **Do not** change `tenantctl.sh` or the verify step. M2/M3 landed there and are reviewed.
- **Do not** weaken step 1 into a skip, and **do not** collapse M4's could-not-run / check-failed
  split. It must fail closed.
- Shell only in Task 1; Go only in Task 2. `#!/bin/sh` — **no bashisms**.
- `test/arch` stays green — six onboarding guards.

## Task 1 — Assert the named tables, in the named database

**File:** `kanz/infra/onboarding/provision-tenant.sh` (step 1).

**Requirements:**

1. **Target the database explicitly.** Add `-d "$DB_NAME"` with `DB_NAME="${DB_NAME:-app}"`, and a
   comment stating WHY `app`: CNPG's default initdb creates it because `cluster.yaml` declares no
   `bootstrap` block. Overridable, because a future `bootstrap.initdb.database` would change it and
   this script must not silently query the wrong place again.
2. **Assert the NAMED tables per cluster, by COUNT.** For each cluster, the expected table set and
   its size are declared in the script:
   - `kanz-risk` → `portfolios`, `positions`, `applied_keys` (3)
   - `kanz-books` → `ledger_entries`, `ledger_snapshots` (2)

   Query the count of those named relations having **both** `relrowsecurity` AND
   `relforcerowsecurity`, and require it to equal the expected size. A count is deliberate: an empty
   or malformed result is `0 ≠ 3`, so it cannot pass vacuously — which `[ -n "$out" ]` could, if
   anyone ever added `-t` to the exec and made an empty result `"\r"`.
3. **Name what is missing when it fails.** "2 of 3" is not actionable; the FATAL must say which
   tables lack FORCE RLS, so an operator knows whether it is a migration that did not run or a
   control that was turned off. (A second query, or a query returning the missing names and checking
   emptiness against a *known expected set*, are both fine — your judgement; a bare count in the
   message is not.)
4. **M4's split must survive:** a kubectl/psql failure still yields the distinct "could not run the
   check — this is NOT a verdict on RLS" FATAL, never an RLS verdict.
5. Keep it POSIX `sh`. The per-cluster table lists can be a `case` on `$c`.

**Verification (must be EXECUTED — the verdict paths cannot be, the could-not-run path must be):**
- `sh -n infra/onboarding/provision-tenant.sh` → parses.
- **M4's split still holds, live:** from `kanz/infra/onboarding`, `TENANT=probe STEP=storage sh
  ./provision-tenant.sh` against the real cluster → exits 1, says the check could **not be run**,
  names the `kanz-data` NotFound, and contains **no** RLS verdict. Paste it. Confirm no namespace was
  created.
- Show the constructed SQL for both clusters (echo it or paste it from the source) so a reviewer can
  read what will actually run.
- `go test ./test/arch/... -count=1` green.

## Task 2 — Guard the table lists against the migrations

**File:** `kanz/test/arch/onboarding_test.go` (extend; six guards live there).

**Why:** Task 1 hardcodes table names that the migrations also declare. **That is the same defect
family this whole epic is about** — one fact in two places with nothing comparing them. A migration
adding a tenant-scoped table would leave step 1 certifying an incomplete set, silently.

**Requirements:**

1. Parse the FORCE-RLS table names out of the migrations for the two services step 1 covers —
   `services/risk-engine/migrations/` and `services/accounting/migrations/` — and assert they match
   the lists `provision-tenant.sh` declares for `kanz-risk` and `kanz-books`.
   **The SQL is `FORCE  ROW LEVEL SECURITY` (two spaces), and risk-engine/accounting declare theirs
   inside a `FOREACH t IN ARRAY ARRAY[...]` loop** — so the parse must handle the array form, not just
   `ALTER TABLE x FORCE`.
2. Fail naming the drift in both directions: a table the migrations FORCE that the script does not
   check, and a table the script checks that the migrations do not FORCE.
3. **Non-vacuous:** if either side parses to an empty set, that is a FAILURE, not a pass — an empty
   expected set would make the count assertion trivially satisfiable.
4. Reuse `moduleRoot(t)`; normalise CRLF; follow the file's existing style.

**Verification (must be EXECUTED):**
- Passes after Task 1.
- **Mutation A:** remove one table from the script's list → FAILS naming it. Restore, confirm green.
- **Mutation B:** add a bogus table to the script's list → FAILS naming it. Restore, confirm green.
- `go test ./test/arch/... -count=1` green; `gofmt -l .` clean; `git status --short` shows no modified
  tracked files under `kanz/`.
