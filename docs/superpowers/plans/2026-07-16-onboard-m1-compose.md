# ONBOARD-M1 — one onboarding path: the golden path composes the lifecycle

## Context

Onboarding a tenant has **two** automations. Neither is complete, they duplicate one step, and they
contradict each other on another:

- **Path A — `kanz/infra/onboarding/provision-tenant.sh` (PARITY-06e, `/bin/sh`, 102 lines).**
  6 steps: verify RLS · **broker: "tenant stamping — no per-tenant stream needed"** · AUTH-01b policy
  bundle · **quota** · seed first portfolio · verify cross-tenant isolation.
- **Path B — `kanz/infra/tenancy/tenantctl.sh` (MT-01f, `bash`, 234 lines).**
  `onboard` runs identity (namespace + ServiceAccounts ⇒ SPIRE SVIDs) · **broker: NATS account +
  Kafka prefixed topics/ACLs** · state (Postgres per-tenant login role) · **quota**. `offboard`
  reverses the order. Path A has no offboard.

**Path A's broker step is now FALSE, and SEC-M3 is why.** "No per-tenant stream needed" was true when
the broker took plaintext. The production broker now runs `tls { verify: true, verify_and_map: true }`
and maps an SVID to a NATS account — so a tenant onboarded by Path A alone gets **no namespace, no
SVIDs, no NATS account**: its workloads complete the handshake, map to no account, and cannot publish
or subscribe. That is the SEC-M3b defect one level up.

**The quota duplication is concrete, not theoretical.** Both write the SAME ConfigMap
(`api-gateway-quotas` in `kanz-services`) with the same JSON shape — and **different defaults**:
A is `200/400/100`, B is `50/100/64`. Whichever ran last silently decides a tenant's rate limit.

**The decision (lead, 2026-07-16): Option 1 — A composes B.** They are not redundant: **B is the
infrastructure lifecycle, A is the client golden path.** A keeps what only it does (policy bundle,
seed, isolation verify), delegates the infrastructure half to B, and gains B's offboard by reference.
One entry point for an operator.

## Ground truth — verified by the controller, do not re-derive

- The gateway does **not** trust a raw `X-Kanz-Tenant` header; it injects `X-Kanz-Principal-Tenant`
  from the authenticated principal (`services/api-gateway/internal/proxy/backend.go`).
- **Neither script has an exec bit** (both `100644`), so `./provision-tenant.sh` does not run as its
  own usage line documents, and Path A cannot invoke Path B as `./tenantctl.sh`.
- Path A is `#!/bin/sh`; Path B is `#!/usr/bin/env bash` with `set -euo pipefail`. **A must invoke B
  as `bash <path> onboard`** — `sh tenantctl.sh` would break on its bashisms.
- Path A resolves siblings relative to its own directory (`../security/policies/...`,
  `../../test/load/seed`), i.e. it assumes cwd = `infra/onboarding`.
- B's entrypoint is `case "${1:-}" in onboard) ... offboard) ...`, and it requires `TENANT`.
- No tenant has ever been onboarded through either path (every deployment sets `*_TENANT:
  "__system__"`, the reserved platform tenant, which predates these scripts). **So no existing
  tenant's quota changes when B becomes the sole quota owner.**

## Global Constraints

- **Do not** change `tenantctl.sh` (Path B). It is correct and it is the composed dependency. This
  plan only changes the golden path and adds a guard.
- **Do not** attempt to fix Path A's step-6 verify (it sends a header the gateway ignores and no
  bearer token, so it 401s and FATALs on a misdiagnosis). It is a real, separate defect, boarded as
  its own line. Out of scope here — fixing it needs a token-acquisition decision, and bundling it
  would hide a reconciliation behind an auth change.
- Shell only. No Go behaviour changes.
- The existing `test/arch` suite stays green.

## Task 1 — The golden path composes the lifecycle

**File:** `kanz/infra/onboarding/provision-tenant.sh`

**Requirements:**

1. **Delete the false broker step.** The `if step broker; then ... "no per-tenant stream needed" ...`
   block goes. It is not merely redundant now — it is a lie the next operator inherits.
2. **Delete Path A's quota step** (the `if step quota` block, its `RATE`/`BURST`/`INFLIGHT` locals and
   its ConfigMap write). **B is the sole quota owner.** Note in a comment that the quota knobs
   (`RATE_PER_SEC`, `BURST`, `MAX_IN_FLIGHT`) now pass through to `tenantctl.sh`, and that **B's
   defaults (50/100/64) apply, not A's old 200/400/100** — a deliberate consequence of one owner.
3. **Add an `infra` step that invokes Path B**, in place of the two deleted steps and BEFORE the
   policy step: `TENANT="$TENANT" bash ../tenancy/tenantctl.sh onboard`. It must run under A's
   existing `step` gate (`STEP=infra ./provision-tenant.sh` runs only it) and must fail the script if
   B fails (A is `set -eu`, so an unguarded call already does — confirm, don't assume).
4. Renumber the step banners so they are honest (the script becomes 5 steps: storage · infra ·
   policy · seed · verify). A banner that says `[2/6]` in a 5-step script is the same class of defect
   as the broker line.
5. **Update the header comment** to state that this script is the CLIENT golden path and that it
   delegates infrastructure to `tenantctl.sh` — and that offboarding is `tenantctl.sh offboard`
   (Path A has no offboard and must not grow one).
6. Set the **exec bit** on `provision-tenant.sh` and `tenantctl.sh` (`git update-index --chmod=+x`),
   so both run as their own usage lines document.

**Explicitly out of scope:** the step-6 verify defect (see Global Constraints); any change to
`tenantctl.sh`.

**Verification (must be EXECUTED, output pasted verbatim — no cluster here, so verify what can be):**
- `sh -n infra/onboarding/provision-tenant.sh` and `bash -n infra/tenancy/tenantctl.sh` → both parse.
- `grep -n "no per-tenant stream needed\|api-gateway-quotas" infra/onboarding/provision-tenant.sh` →
  **no matches** (the lie and the duplicate are gone).
- `grep -n "tenantctl.sh" infra/onboarding/provision-tenant.sh` → the composition exists.
- `git ls-files -s` on both scripts → mode `100755`.
- Step banners are self-consistent (`[N/5]`, N = 1..5, no duplicates).

## Task 2 — Guard the composition

**File:** `kanz/test/arch/onboarding_test.go` (new), package `arch`.

**Why:** the root cause of ONBOARD-M1 was that **nothing compared the two scripts**. They drifted
until one contradicted the other and SEC-M3 made the contradiction dangerous. A comment saying "B owns
quota" decays; a test does not.

**Requirements:**

1. Assert `infra/onboarding/provision-tenant.sh` **invokes** `tenantctl.sh` — the composition is real,
   not aspirational.
2. Assert the golden path does **not** write the quota ConfigMap (`api-gateway-quotas`) — B is the
   sole owner. Re-adding a quota write to A fails the build.
3. Assert the golden path does **not** contain the retired claim `no per-tenant stream needed` — the
   specific false statement SEC-M3 invalidated, pinned so it cannot return.
4. **Non-vacuous:** if either script is missing or unreadable, that is a FAILURE, not a skip.
5. Reuse `moduleRoot(t)`. Note the scripts live under `kanz/infra/...`, i.e. inside the module root.
6. House style: the doc comment explains WHY the guard exists and what broke without it; failure
   messages tell the reader what to DO.

**Verification (must be EXECUTED, output pasted verbatim):**
- Passes on the tree after Task 1.
- **Mutation A:** re-add a line containing `no per-tenant stream needed` to the golden path → FAILS
  naming it. Restore (targeted edit, not `git checkout -- .`), confirm green.
- **Mutation B:** re-add an `api-gateway-quotas` write to the golden path → FAILS. Restore, confirm.
- **Mutation C:** remove the `tenantctl.sh` invocation → FAILS. Restore, confirm.
- `go test ./test/arch/... -count=1` green; `gofmt -l .` clean; `git status --short` shows no modified
  tracked files when finished.
