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
# provisioning anything), and jq (the gateway quota step reads/writes the quota
# ConfigMap through it). No DB credential is needed to onboard: nothing is
# provisioned per tenant in Postgres (see step 1 below).
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
# RISK_DB_NAME / BOOKS_DB_NAME: the database step 1's RLS check queries, one
# per cluster (kanz-risk, kanz-books) — pg_class is per-database, so querying
# the wrong one returns nothing and step 1 misdiagnoses a HEALTHY cluster as
# "MT-01d not deployed". There is NO default, deliberately: infra/dr/postgres/
# README.md and cluster.yaml's own comment say this platform hosts "one
# database per service", but cluster.yaml's spec declares no bootstrap/initdb
# block for either cluster — the manifest contradicts its own comment, so
# CNPG's actual default database name ("app") is not something this repo
# agrees on, and nothing under infra/ creates a per-service database anyway
# (no postInitSQL, no CREATE DATABASE). A prior version of this script
# defaulted to "app"; that was a guess, and a wrong guess produces the exact
# false "MT-01d not deployed" alarm on a healthy cluster this step exists to
# prevent. So: no fallback. The operator supplies both names, read off the
# real DSN each service's deployment mounts — RISK_ENGINE_DATABASE_URL_FILE
# (services/risk-engine, cluster kanz-risk) and ACCOUNTING_DATABASE_URL_FILE
# (services/accounting, cluster kanz-books), both at
# /run/secrets/db/database-url in their respective Deployments. Step 1
# refuses (exit 2) before any kubectl runs if either is unset.
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
#
#    ONBOARD-M5: the query now names the NAMED tenant-scoped tables per cluster
#    (below), not `limit 1` on any relation anywhere in the database. `limit 1`
#    proved RLS was on SOMEWHERE — if `positions` alone lost FORCE RLS,
#    `portfolios` still satisfied `limit 1` and this step reported isolation
#    active while positions leaked across tenants. It also now connects with
#    `-d "$db"` (RISK_DB_NAME/BOOKS_DB_NAME, see the header above) instead of
#    psql's default "postgres" database, where these tables do not exist.
#
#    The query returns the FORCE-RLS relnames among the named tables, and the
#    OUTPUT is the verdict, not the exit status — `psql -tAc` exits 0 for a
#    successful query that returns zero rows, so a version of this check that
#    piped the query to `>/dev/null` and branched on `$?` alone could never tell
#    "RLS is off" from "RLS is on": both are a successful query, so both exit 0
#    and both pass. The output is captured into `present` below and compared,
#    by NAME, against the expected set for exactly the reason
#    app_current_tenant() RAISES instead of returning NULL (KANZ_BRAIN.md): an
#    empty or partial result and a broken query must never be the same
#    observable event. Do NOT "tidy" the capture back into `>/dev/null` — that
#    silently restores the vacuous check, and do NOT go back to `limit 1` or a
#    bare count — a count alone cannot say WHICH table lost FORCE RLS.
# The tenant-scoped tables this step KNOWINGLY does not verify, and why.
#
# NINE services declare FORCE ROW LEVEL SECURITY; this step checks the five
# tables in kanz-risk and kanz-books. It used to check those five and say
# nothing about the rest, so a tenant was handed over as "isolation verified"
# with most of the tenant-scoped estate never confirmed live — under-coverage
# the operator had no way to see.
#
# These are NOT verified here because ONBOARD-M6 is unresolved: the repo gives
# several answers for which DATABASE (and, for oms/tv-sync/venue-*, which
# CNPG CLUSTER) each service's tables live in, and this script refuses to guess
# one — a wrong guess reports a HEALTHY cluster as "MT-01d not deployed",
# which is the exact harm step 1 exists to catch, merely relocated. Resolve M6
# (`vault kv get -field=dsn kv/kanz/<service>`), then move the table into a
# verified cluster arm above and delete it from this list.
#
# test/arch TestProvisionTenantAccountsForEveryTenantScopedTable fails the build
# if a table is in neither this list nor a verified arm — a new RLS'd table
# cannot silently widen the unchecked surface, it has to be triaged. Same shape
# as the archiver's unbackedByDesign map.
UNVERIFIED_RLS_TABLES="oms:orders,positions,position_fills,outbox,order_proposals alternatives:fund_events datamaster:golden_records,exceptions,exception_overrides,exception_override_proposals,outbox wealth:households tv-sync:tv_facts venue-binance:venue_orders venue-okx:venue_orders"

if step storage; then
  echo "-- [1/6] verify RLS isolation is active (kanz-risk, kanz-books)"

  # REFUSE before any kubectl runs if either database name is unset. This
  # script will not guess: see the RISK_DB_NAME/BOOKS_DB_NAME header comment
  # above for why there is no default — a wrong guess produces a false
  # "MT-01d not deployed" on a HEALTHY cluster, which is the exact harm this
  # step exists to catch, merely relocated to the wrong database.
  db_missing=""
  [ -n "${RISK_DB_NAME:-}" ] || db_missing="$db_missing  - RISK_DB_NAME (the kanz-risk database name — read it off RISK_ENGINE_DATABASE_URL_FILE, /run/secrets/db/database-url in the risk-engine Deployment)
"
  [ -n "${BOOKS_DB_NAME:-}" ] || db_missing="$db_missing  - BOOKS_DB_NAME (the kanz-books database name — read it off ACCOUNTING_DATABASE_URL_FILE, /run/secrets/db/database-url in the accounting Deployment)
"
  if [ -n "$db_missing" ]; then
    echo "REFUSED: cannot verify RLS isolation — missing prerequisite(s):" >&2
    printf '%s' "$db_missing" >&2
    echo "This script will not guess a database name: a wrong guess reports a HEALTHY cluster as \"MT-01d not deployed\" instead of refusing. Supply both and retry." >&2
    exit 2
  fi

  for c in kanz-risk kanz-books; do
    # The expected FORCE-RLS table set per cluster, read off the migrations
    # that declare it (services/risk-engine/migrations/0002_tenant_rls.sql;
    # services/accounting/migrations/0001_ledger.sql — note the SQL there is
    # `FORCE  ROW LEVEL SECURITY` with TWO SPACES, in a `FOREACH t IN ARRAY
    # ARRAY[...]` loop, not individual `ALTER TABLE x FORCE` statements).
    case "$c" in
      kanz-risk)  tables="portfolios positions applied_keys"; db="$RISK_DB_NAME" ;;
      kanz-books) tables="ledger_entries ledger_snapshots"; db="$BOOKS_DB_NAME" ;;
      *) echo "FATAL: no table set declared for $c" >&2; exit 1 ;;
    esac
    expected="$(set -- $tables; echo $#)"

    in_list=""
    for t in $tables; do
      in_list="${in_list:+$in_list,}'$t'"
    done

    present="$(k -n "$DATA_NS" exec "$c-1" -- psql -d "$db" -tAc \
      "select relname from pg_class where relname in ($in_list) and relrowsecurity and relforcerowsecurity order by relname")" \
      || { echo "FATAL: could not run the RLS check on $c — kubectl exec or psql failed (see the error above); this is NOT a verdict on RLS, it means the check could not be run. Fix the underlying infrastructure (namespace/pod/DB reachability) and retry" >&2; exit 1; }

    missing=""
    found_count=0
    for t in $tables; do
      found=0
      old_ifs="$IFS"
      IFS='
'
      for line in $present; do
        [ "$line" = "$t" ] && found=1
      done
      IFS="$old_ifs"
      if [ "$found" -eq 1 ]; then
        found_count=$((found_count + 1))
      else
        missing="$missing $t"
      fi
    done

    [ "$found_count" -eq "$expected" ] || { echo "FATAL: FORCE RLS not active on $c for:$missing (expected $expected of {$tables}, database '$db') — MT-01d not deployed for the named table(s), or FORCE RLS was turned off" >&2; exit 1; }
  done

  # State the SCOPE of what just passed. This step verifies five tables; the
  # tenant-scoped estate is larger, and an operator reading "RLS isolation is
  # active" was previously entitled to assume it covered everything.
  echo "   VERIFIED: FORCE RLS active on risk-engine{portfolios,positions,applied_keys} + accounting{ledger_entries,ledger_snapshots}"
  echo "   NOT VERIFIED by this step (ONBOARD-M6: database/cluster unresolved, and this script will not guess):" >&2
  for group in $UNVERIFIED_RLS_TABLES; do
    echo "     - ${group%%:*}: $(printf '%s' "${group#*:}" | tr ',' ' ')" >&2
  done
  echo "   These tables' migrations DO declare FORCE RLS — this is an unverified claim, not a known failure. Resolve ONBOARD-M6 to close the gap." >&2
fi

# 2) Infrastructure lifecycle (MT-01c/d/e via tenantctl.sh): identity (namespace
#    + ServiceAccounts + SPIFFE), broker isolation (NATS account + Kafka
#    prefixed topics/ACLs), and the gateway per-tenant quota entry. State needs
#    no per-tenant provisioning at all — isolation is FORCE RLS + the
#    app.tenant_id GUC on shared tables (see the storage step above).
#    tenantctl.sh is the sole owner of all three — this
#    step composes it rather than re-implementing any of it. Quota knobs
#    (RATE_PER_SEC, BURST, MAX_IN_FLIGHT) pass through to tenantctl.sh; if unset,
#    B's defaults apply (50/s, burst 100, max-in-flight 64) — NOT Path A's old
#    200/400/100, a deliberate consequence of B being the one quota owner.
if step infra; then
  echo "-- [2/6] provision infrastructure lifecycle for $TENANT (tenantctl.sh onboard)"
  TENANT="$TENANT" bash ../tenancy/tenantctl.sh onboard
fi

# 3) Compute (MT-02): render the tenant's OMS manifest to
#    kanz/infra/deploy/tenants/$TENANT/oms-$TENANT.yaml. Placed right after
#    `infra`, before `policy`: compute is worthless without the identity
#    tenantctl.sh's infra step just established, and a tenant should never be
#    granted a policy bundle (step 4) for a service that has no process to
#    serve it yet. This step only WRITES the file — it is plain YAML under
#    infra/deploy/, which the ApplicationSet's `workloads` component already
#    raw-syncs with prune:true + selfHeal:true, so anything applied here
#    out-of-band would come up and then be PRUNED minutes later because it is
#    not in git. Committing the rendered file is what deploys it, not this
#    script.
if step compute; then
  echo "-- [3/6] render per-tenant compute manifest for $TENANT (kanz/infra/deploy/tenants/$TENANT/)"

  # REFUSE before writing anything — same preflight-first stance as
  # tenantctl.sh: name every missing prerequisite in one pass.
  compute_missing=""
  if [ "$TENANT" = "__system__" ]; then
    compute_missing="$compute_missing  - TENANT is '__system__' — that is the reserved platform tenant (infra/nats/tenancy.yaml), never a real tenant; choose a different TENANT id
"
  fi
  [ -f "../deploy/oms-deploy.yaml" ] || compute_missing="$compute_missing  - ../deploy/oms-deploy.yaml not found — the base this step renders is missing; the checkout looks incomplete
"
  if [ -n "$compute_missing" ]; then
    echo "REFUSED: cannot render compute manifest for '$TENANT' — missing prerequisite(s):" >&2
    printf '%s' "$compute_missing" >&2
    exit 2
  fi

  # internal/tenantgen.Render is the ONE generator — this is the same code
  # path test/arch/tenant_compute_test.go's drift guard calls to re-render
  # every committed tenant manifest from the live base. Deterministic: a
  # rerun with an unchanged base always overwrites with the byte-identical
  # result, never a guess about whether an existing copy is stale.
  OUT="../deploy/tenants/$TENANT/oms-$TENANT.yaml"
  ( cd ../.. && go run ./cmd/kanz-tenantgen -tenant "$TENANT" \
      -base infra/deploy/oms-deploy.yaml \
      -out "infra/deploy/tenants/$TENANT/oms-$TENANT.yaml" )
  echo "   wrote $OUT"

  # NATS user (SEC-M3): the OMS pod's SPIFFE ID is derived from its
  # ServiceAccount, not from tenantctl.sh's per-tenant-namespace TENANT_SAS
  # pattern (compute lives in kanz-services, not tenant-$TENANT — ground
  # truth of MT-02's design). tenantctl.sh's own onboard_nats already
  # documents the static/manual path for exactly this shape of edit
  # (nats/tenancy.yaml's header: "Add a tenant by appending an account + user
  # keyed on the tenant workload's SPIFFE URI SAN, then 'nats-server --signal
  # reload'") — follow it here rather than inventing a second mechanism.
  OMS_PRINCIPAL="spiffe://kanz.internal/ns/kanz-services/sa/oms-${TENANT}"
  echo ""
  echo "   MANUAL (same static-mode path as tenantctl.sh's onboard_nats): add BOTH"
  echo "   blocks to tenant $TENANT's account in infra/nats/tenancy.yaml:"
  echo ""
  echo "     users: [ { user: \"${OMS_PRINCIPAL}\" } ]"
  echo ""
  echo "     imports: ["
  echo "       { stream: { account: __system__, subject: \"tenant.${TENANT}.order.order.submit\" }"
  echo "         to: \"order.order.submit\" }"
  echo "       { stream: { account: __system__, subject: \"tenant.${TENANT}.order.order.cancel\" }"
  echo "         to: \"order.order.cancel\" }"
  echo "     ]"
  echo ""
  echo "   then 'nats-server --signal reload'."
  echo ""
  echo "   BOTH ARE REQUIRED, and they fail differently (MT-02, #358/#360):"
  echo "     - without the USER the pod authenticates and reaches NO account (SEC-M3);"
  echo "     - without the IMPORTS it reaches its account and receives NOTHING. The"
  echo "       gateway and webhook-ingest publish tenant.${TENANT}.order.order.submit"
  echo "       into __system__, and accounts are isolated by construction — so the"
  echo "       pod sits idle, which looks exactly like a tenant that is not trading."
  echo ""
  echo "   Both are build-enforced: test/arch/tenant_compute_test.go fails if the user"
  echo "   is missing, tenant_bridge_test.go if the imports are missing or name"
  echo "   another tenant's prefix."

  echo ""
  echo "   THIS SCRIPT DID NOT DEPLOY ANYTHING. NEXT STEP: commit $OUT to main —"
  echo "     git add $OUT && git commit -m 'tenant $TENANT: compute (MT-02)'"
  echo "   The commit — not this script — is what deploys the tenant's OMS."
fi

# 4) AUTH-01b policy bundle: install the tenant's role→permission bundle. The
#    shape is the shipped infra/security/policies/risk-authz.json (policy-as-data,
#    ConfigMap-mounted). A tenant gets the standard roles; the client's admin is
#    granted risk.admin, analysts risk.analyst, everyone else risk.reader.
if step policy; then
  echo "-- [4/6] install AUTH-01b policy bundle for $TENANT"
  k -n "$SVC_NS" create configmap "authz-$TENANT" \
    --from-file=risk-authz.json=../security/policies/risk-authz.json \
    --dry-run=client -o yaml | k apply -f -
  echo "   admin subject: $ADMIN_SUBJECT → risk.admin (grant via the IdP group→role mapping)"
fi

# 4) Data-source binding + first portfolio: bind the tenant's market/reference feed
#    entitlements (composition-root env on the market-data/datamaster adapters) and
#    seed a first portfolio so the read path serves real data on day one.
if step seed; then
  echo "-- [5/6] seed first portfolio for $TENANT"
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
  echo "-- [6/6] verify cross-tenant isolation"
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
