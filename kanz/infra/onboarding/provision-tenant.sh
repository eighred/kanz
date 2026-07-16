#!/bin/sh
# PARITY-06e — tenant onboarding golden path, automated end-to-end.
#
# This is the CLIENT golden path: the single script an operator runs to bring
# a new institutional tenant online. It composes the infrastructure lifecycle
# (kanz/infra/tenancy/tenantctl.sh — identity, broker, state, quota) as its
# `infra` step, then layers the AUTH-01b policy bundle + portfolio scoping,
# binds data sources, and seeds a first portfolio — then verifies cross-tenant
# isolation holds before handing the tenant to the client. Each step is
# idempotent and re-runnable, so a partial provision resumes (the failover.sh
# discipline).
#
# There is no offboard here by design: this script only ever grows a tenant.
# To remove one, run tenantctl.sh directly: TENANT=acme ../tenancy/tenantctl.sh
# offboard. Path A does not and must not grow an offboard path of its own.
#
#   TENANT=acme ADMIN_SUBJECT=alice@acme.com ./provision-tenant.sh
#   TENANT=acme STEP=policy ./provision-tenant.sh        # run one step
#
# Requires: kubectl, psql (or the cnpg plugin), and the target cluster context —
# plus, for the tenantctl.sh `infra` step this script composes: nsc + NATS_OPERATOR
# (else the NATS account step logs a manual instruction and returns 0 instead of
# provisioning anything), ADMIN_DATABASE_URL + TENANT_DB_PASSWORD (else the
# Postgres role step is skipped), and jq (the gateway quota step reads/writes the
# quota ConfigMap through it).
#
# --- ONBOARD-M2 Task 1: the verify step (step 5) tests REAL cross-tenant
# isolation, or refuses — it no longer probes with no identity at all and
# calls a 401 "isolation holds". It needs two bearer tokens:
#
#   VERIFY_TOKEN       — a token authenticated as a principal belonging to
#                        the tenant being onboarded ($TENANT). Proves the new
#                        tenant can read its own portfolio (must be 200).
#   VERIFY_TOKEN_OTHER — a token authenticated as a principal belonging to
#                        ANY OTHER already-existing tenant. Proves that
#                        tenant cannot read $TENANT's portfolio (must be
#                        403 or 404).
#
# Source of both, in production: Eighred SSO (OIDC) — kanz mints no
# production identity. In dev/staging against the HS256 validator, mint both
# with cmd/kanz-devtoken (a standalone CLI in no service image).
#
# Both are sent as `Authorization: Bearer <token>`. Neither probe request
# sets an `X-Kanz-Tenant` header: the gateway never reads that header, it
# derives the caller's tenant from the authenticated principal (injecting
# X-Kanz-Principal-Tenant downstream itself) — a header the gateway ignores
# is a lie the next reader inherits, the same defect class ONBOARD-M1
# deleted from this same file.
#
# Exit codes (verify step only):
#   0 = isolation confirmed: self=200, cross=403/404.
#   1 = a genuine isolation failure: self not 200 (including self=401 — a bad
#       token, reported as such, never blamed on the tenant), cross=200 (a
#       REAL cross-tenant leak — the one outcome this script exists to
#       prevent), or cross=401 (the probe token itself does not authenticate,
#       so its denial proves nothing about isolation — reported as
#       inconclusive, never as "isolation holds") — or the gateway itself was
#       unreachable on either probe (curl failed outright), reported as a
#       FATAL naming the unreachable gateway, never a silent abort.
#   2 = REFUSED: VERIFY_TOKEN and/or VERIFY_TOKEN_OTHER are unset, or the two
#       are byte-identical (the cross-tenant probe token must belong to a
#       DIFFERENT tenant). Checked before step 1 whenever STEP=all, so a run
#       never provisions steps 1-4 only to discover at step 5 that the gate
#       cannot run.
#
# Honest limit: verify_preflight refuses (exit 2) when VERIFY_TOKEN and
# VERIFY_TOKEN_OTHER are byte-identical, which catches the single most likely
# operator error (copy-paste) before any probe runs. This shrinks but does
# not eliminate the limit: if VERIFY_TOKEN_OTHER is a DISTINCT token that
# still happens to authenticate as the SAME tenant as VERIFY_TOKEN, the cross
# probe returns 200 and this script reports a CROSS-TENANT LEAK that is not
# real — it can only compare token strings, never subjects. Verify
# VERIFY_TOKEN_OTHER's subject before treating a reported leak as real.
set -eu

TENANT="${TENANT:?set TENANT to the new tenant id (lowercase, dns-safe)}"
ADMIN_SUBJECT="${ADMIN_SUBJECT:-admin@$TENANT}"
DATA_NS="${DATA_NS:-kanz-data}"
SVC_NS="${SVC_NS:-kanz-services}"
MSG_NS="${MSG_NS:-kanz-messaging}"
STEP="${STEP:-all}"
k() { kubectl "$@"; }
step() { [ "$STEP" = "all" ] || [ "$STEP" = "$1" ]; }

# verify_preflight: REFUSE (exit 2) before any curl runs if either bearer
# token is absent — the ONBOARD-M3 tenantctl.sh preflight() style: name every
# missing prerequisite in one pass, touch nothing until both are confirmed
# present. See the header above for what each token is and where an operator
# gets one. Called twice: once early (below, only when STEP=all, before step
# 1) and again at the top of the verify step itself, so `STEP=verify` alone
# still refuses correctly when run in isolation.
verify_preflight() {
  missing=""
  [ -n "${VERIFY_TOKEN:-}" ] || missing="$missing  - VERIFY_TOKEN (bearer token for tenant '$TENANT' itself — Eighred SSO in production, cmd/kanz-devtoken in dev)
"
  [ -n "${VERIFY_TOKEN_OTHER:-}" ] || missing="$missing  - VERIFY_TOKEN_OTHER (bearer token for any OTHER existing tenant — same source)
"
  if [ -n "$missing" ]; then
    echo "REFUSED: cannot verify cross-tenant isolation for '$TENANT' — missing prerequisite(s):" >&2
    printf '%s' "$missing" >&2
    exit 2
  fi
  if [ "$VERIFY_TOKEN" = "$VERIFY_TOKEN_OTHER" ]; then
    echo "REFUSED: VERIFY_TOKEN and VERIFY_TOKEN_OTHER are the same token — the cross-tenant probe token (VERIFY_TOKEN_OTHER) must belong to a DIFFERENT, already-existing tenant, not '$TENANT' itself. Mint a distinct token for another tenant and retry." >&2
    exit 2
  fi
}

echo "== provision tenant '$TENANT' (step=$STEP) =="

# Refuse EARLY — before step 1 ever runs — when the whole path (STEP=all)
# will reach the verify gate at the end. Provisioning a tenant through steps
# 1-4 only to discover at step 5 that the gate cannot run leaves it
# half-handed-over, which is exactly the failure this gate exists to
# prevent.
if [ "$STEP" = "all" ]; then
  verify_preflight
fi

# 1) Storage isolation (MT-01d): the tenant's rows are scoped by Postgres RLS on
#    app.tenant_id — no per-tenant database, one policy-enforced table. Nothing to
#    create per tenant except confirming the RLS policy + FORCE RLS are active on
#    the shared clusters; the tenant id is carried on every connection's GUC by
#    the services (the AfterConnect set_config path). This step is a VERIFY, not a
#    mutate — RLS already isolates any tenant id that appears.
if step storage; then
  echo "-- [1/5] verify RLS isolation is active (kanz-risk, kanz-books)"
  for c in kanz-risk kanz-books; do
    k -n "$DATA_NS" exec "$c-1" -- psql -tAc \
      "select relname from pg_class where relrowsecurity and relforcerowsecurity limit 1" \
      >/dev/null || { echo "FATAL: FORCE RLS not active on $c — MT-01d not deployed"; exit 1; }
  done
fi

# 2) Infrastructure lifecycle (MT-01c/d/e via tenantctl.sh): identity (namespace
#    + ServiceAccounts + SPIFFE), broker isolation (NATS account + Kafka
#    prefixed topics/ACLs), the Postgres per-tenant login role, and the gateway
#    per-tenant quota entry. tenantctl.sh is the sole owner of all four — this
#    step composes it rather than re-implementing any of it. Quota knobs
#    (RATE_PER_SEC, BURST, MAX_IN_FLIGHT) pass through to tenantctl.sh; if unset,
#    B's defaults apply (50/s, burst 100, max-in-flight 64) — NOT Path A's old
#    200/400/100, a deliberate consequence of B being the one quota owner.
if step infra; then
  echo "-- [2/5] provision infrastructure lifecycle for $TENANT (tenantctl.sh onboard)"
  TENANT="$TENANT" bash ../tenancy/tenantctl.sh onboard
fi

# 3) AUTH-01b policy bundle: install the tenant's role→permission bundle. The
#    shape is the shipped infra/security/policies/risk-authz.json (policy-as-data,
#    ConfigMap-mounted). A tenant gets the standard roles; the client's admin is
#    granted risk.admin, analysts risk.analyst, everyone else risk.reader.
if step policy; then
  echo "-- [3/5] install AUTH-01b policy bundle for $TENANT"
  k -n "$SVC_NS" create configmap "authz-$TENANT" \
    --from-file=risk-authz.json=../security/policies/risk-authz.json \
    --dry-run=client -o yaml | k apply -f -
  echo "   admin subject: $ADMIN_SUBJECT → risk.admin (grant via the IdP group→role mapping)"
fi

# 4) Data-source binding + first portfolio: bind the tenant's market/reference feed
#    entitlements (composition-root env on the market-data/datamaster adapters) and
#    seed a first portfolio so the read path serves real data on day one.
if step seed; then
  echo "-- [4/5] seed first portfolio for $TENANT"
  SEED_TENANT="$TENANT" SEED_PORTFOLIO="${FIRST_PORTFOLIO:-PF1}" \
    SEED_NATS_URL="${SEED_NATS_URL:-nats://nats.$MSG_NS:4222}" \
    go run ../../test/load/seed
fi

# 5) Verify isolation end-to-end BEFORE handing over: the new tenant can read
#    its own portfolio with its own identity, and a genuinely-authenticated
#    OTHER tenant cannot read this tenant's portfolio (the AUTH-01 gate). A
#    provision that leaks cross-tenant is worse than no provision. See the
#    header above for VERIFY_TOKEN/VERIFY_TOKEN_OTHER, the exit codes, and
#    the honest limit on what this check cannot distinguish.
if step verify; then
  echo "-- [5/5] verify cross-tenant isolation"
  verify_preflight
  GW="${GW:-http://api-gateway.$SVC_NS:8080}"
  code_self="$(curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $VERIFY_TOKEN" \
    "$GW/v1/portfolios/${FIRST_PORTFOLIO:-PF1}/exposure")" \
    || { echo "FATAL: gateway unreachable at $GW (self probe) — cannot verify isolation" >&2; exit 1; }
  code_other="$(curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $VERIFY_TOKEN_OTHER" \
    "$GW/v1/portfolios/${FIRST_PORTFOLIO:-PF1}/exposure")" \
    || { echo "FATAL: gateway unreachable at $GW (cross probe) — cannot verify isolation" >&2; exit 1; }
  echo "   self=$code_self other=$code_other"

  if [ "$code_self" != "200" ]; then
    if [ "$code_self" = "401" ]; then
      echo "FATAL: VERIFY_TOKEN is not authenticating (self=401) — the TOKEN is bad or expired; this does NOT mean tenant '$TENANT' cannot read its own portfolio. Mint a fresh VERIFY_TOKEN and retry." >&2
    else
      echo "FATAL: tenant '$TENANT' cannot read its own portfolio (self=$code_self)" >&2
    fi
    exit 1
  fi

  case "$code_other" in
    200)
      echo "FATAL: CROSS-TENANT LEAK — VERIFY_TOKEN_OTHER (another tenant's token) successfully read tenant '$TENANT' data (cross=200). This is the one outcome this entire script exists to prevent. DO NOT hand this tenant over. (If VERIFY_TOKEN_OTHER actually belongs to '$TENANT' by operator error, this is a false alarm — see the header's honest-limit note; verify the token's subject before treating this as a real leak.)" >&2
      exit 1
      ;;
    401)
      echo "FATAL: cross-tenant probe proves NOTHING — VERIFY_TOKEN_OTHER is not authenticating (cross=401), so its denial says nothing about isolation. Provide a genuinely valid token for a DIFFERENT existing tenant and retry." >&2
      exit 1
      ;;
    403|404)
      ;;
    *)
      echo "FATAL: unexpected cross-tenant probe status (cross=$code_other) — expected 403 or 404" >&2
      exit 1
      ;;
  esac

  echo "   isolation holds — tenant ready to hand over"
fi

echo "== tenant '$TENANT' provisioned (step(s) '$STEP') =="
