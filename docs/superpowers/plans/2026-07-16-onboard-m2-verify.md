# ONBOARD-M2 — the handover gate must actually test isolation

## Context

`provision-tenant.sh`'s final step is the gate between a provisioned tenant and a paying client. Its
own comment says *"a provision that leaks cross-tenant is worse than no provision"*. It is **vacuous
by construction**, and has been since SEC-M1:

```sh
code_self="$(curl -s -o /dev/null -w '%{http_code}' -H "X-Kanz-Tenant: $TENANT"      "$GW/v1/portfolios/${FIRST_PORTFOLIO:-PF1}/exposure")"
code_other="$(curl -s -o /dev/null -w '%{http_code}' -H "X-Kanz-Tenant: __other__"   "$GW/v1/portfolios/${FIRST_PORTFOLIO:-PF1}/exposure")"
```

Neither request carries a bearer token, and **the gateway does not trust `X-Kanz-Tenant` at all** —
it *injects* `X-Kanz-Principal-Tenant` from the authenticated principal
(`services/api-gateway/internal/proxy/backend.go`). So neither request has a tenant identity. Today
both return 401 and the step FATALs with *"tenant cannot read its own portfolio (401)"* — misdiagnosing
its own missing auth as a broken tenant.

The deeper defect outlives the 401: **even passing, it would prove nothing.** A `403` on the `other`
probe would only show the gateway rejects anonymous requests — not that tenant B cannot read tenant
A's data. The check has never tested isolation.

**Decision (lead, 2026-07-16): two operator-supplied tokens.** kanz mints no production credential
(`KANZ_BRAIN.md`: *"kanz holds no signing key, issues nothing, and only VALIDATES"*), so the operator
supplies real identity.

## Ground truth — verified by the controller, do not re-derive

- The gateway ignores `X-Kanz-Tenant`; it injects `X-Kanz-Principal-*` from the principal.
- Since SEC-M1 the gateway has **no anonymous mode** — it refuses to start without a credential, so
  an unauthenticated request is 401, always.
- **401 vs 403 is the non-vacuity lever, and it comes for free.** An expired/garbage token is *not
  authenticated* ⇒ **401**. A valid token that is not entitled ⇒ **403/404**. So a `403/404` on the
  cross probe *only happens if the other token is genuinely live*. No third parameter is needed to
  prove the probe token works — but the check must treat **401 as "this proves nothing"**, not as
  "isolation held".
- ONBOARD-M3 established the house stance in this exact file's dependency: a gate must **REFUSE**
  (exit 2) when it cannot do its job — never skip, never lie.
- `STEP=verify` gates off steps 1–4, so the verify step can be driven in isolation with no cluster —
  which is what makes Task 2 testable here.

## Global Constraints

- **Do not** change `tenantctl.sh`. ONBOARD-M3 just landed there; this is the golden path only.
- **Do not** weaken the check into a skip. If the tokens are absent it REFUSES. "Nothing to check"
  and "checked, and fine" must never be the same observable event.
- Shell only in Task 1; Go only in Task 2.
- `test/arch` stays green, including ONBOARD-M1's static guard and ONBOARD-M3's runtime guard.

## Task 1 — Verify with real identity, or refuse

**File:** `kanz/infra/onboarding/provision-tenant.sh` (the verify step, and its header).

**Requirements:**

1. **Two tokens, named and documented:** `VERIFY_TOKEN` (a bearer token for the tenant being
   onboarded) and `VERIFY_TOKEN_OTHER` (a bearer token for **any other existing tenant**). Both are
   sent as `Authorization: Bearer <token>`.
2. **Delete the `X-Kanz-Tenant` header.** The gateway ignores it. A header that does nothing, in the
   check that gates client handover, is a lie the next reader inherits — the same defect class as the
   broker claim ONBOARD-M1 deleted.
3. **The three outcomes must be distinguished, and this is the substance of the task:**
   - `self` (VERIFY_TOKEN → the new tenant's own portfolio) must be **200**. Anything else fails,
     and the message must not blame the tenant when the cause is auth: **401 here means the token is
     bad**, not that the tenant cannot read its portfolio.
   - `cross` (VERIFY_TOKEN_OTHER → the **new tenant's** portfolio) must be **403 or 404**.
     - **200 ⇒ CROSS-TENANT LEAK.** Fail loudly and say exactly that — it is the one outcome this
       whole script exists to prevent.
     - **401 ⇒ the probe proves NOTHING.** `VERIFY_TOKEN_OTHER` is not authenticating, so its denial
       is meaningless. Fail with that diagnosis — never report "isolation holds".
4. **Refuse when it cannot run:** absent `VERIFY_TOKEN` or `VERIFY_TOKEN_OTHER`, the verify step
   exits **2** naming what is missing, in the ONBOARD-M3 style. It must not skip.
5. **Refuse EARLY when the whole path will run.** When `STEP=all`, check for both tokens *before*
   step 1 — provisioning a tenant and only then discovering the gate cannot run leaves a tenant
   half-handed-over, which is the failure this gate exists to prevent.
6. Header comment: document both env vars, where an operator gets them (Eighred SSO in production;
   `cmd/kanz-devtoken` against the HS256 validator in dev), and the exit codes.
7. **Record the honest limit in a comment:** if `VERIFY_TOKEN_OTHER` belongs to the *same* tenant
   (operator error), `cross` returns 200 and is reported as a leak — a false alarm this check cannot
   distinguish from a real one. Say so rather than let a future reader discover it during an incident.

**Verification (must be EXECUTED — no cluster, so drive the refusal, which is the point):**
- `sh -n infra/onboarding/provision-tenant.sh` → parses.
- **REFUSAL:** `TENANT=probe STEP=verify sh infra/onboarding/provision-tenant.sh` with neither token
  set → **exit 2**, names both missing vars, and output contains **no** `isolation holds`. Paste it.
- Confirm no `curl` ran before the refusal (reason from the code and say so).
- ONBOARD-M1's guard still green: `go test ./test/arch/ -run Onboard -count=1`.

## Task 2 — Prove the gate refuses, executably

**File:** `kanz/test/arch/onboarding_test.go` (extend — ONBOARD-M1's and M3's guards live there).

**Why:** ONBOARD-M3's guard proved `tenantctl.sh` refuses. This proves the *golden path's* gate
refuses. Both exist because a script that parses fine can still lie at runtime — which is precisely
what this gate did for the whole life of the project.

**Requirements:**

1. Execute `sh infra/onboarding/provision-tenant.sh` with `TENANT` set to a probe value, `STEP=verify`,
   and a **controlled env** carrying neither `VERIFY_TOKEN` nor `VERIFY_TOKEN_OTHER`. Assert: exit 2,
   the output names the missing tokens, and it **never** contains `isolation holds`.
2. Do **not** inherit the developer's environment (`exec.Command`'s `Env`) — an engineer with
   `VERIFY_TOKEN` exported would silently change what the test proves.
3. Must not require or touch a cluster or a gateway. **Pin `KUBECONFIG` to a nonexistent path** —
   a real `kind` cluster is reachable from this box and an earlier agent mutated it by accident,
   because bash recomputes `HOME` regardless of the env passed.
4. Non-vacuous: a non-zero exit for the *wrong* reason (127 "no such file", a parse error) must FAIL
   the test, not pass it. Assert on exit **2** specifically.
5. Reuse `moduleRoot(t)`. Follow the file's existing style; skip only if `sh`/`bash` is unavailable.

**Verification (must be EXECUTED):**
- Passes after Task 1.
- **Mutation:** make the verify step skip (return/exit 0) when the tokens are absent → the test
  FAILS. Restore with a targeted edit (never `git checkout -- .`), confirm green.
- `go test ./test/arch/... -count=1` green; `gofmt -l .` clean; `git status --short` shows no
  modified tracked files under `kanz/` when finished.
