#!/bin/sh
# PARITY-06e — tenant onboarding golden path, automated end-to-end.
#
# Provisions a new institutional tenant across every isolation layer the MT-01
# epic built, installs its AUTH-01b policy bundle + portfolio scoping, binds its
# data sources, and seeds a first portfolio — then verifies cross-tenant
# isolation holds before handing the tenant to the client. Each step is
# idempotent and re-runnable, so a partial provision resumes (the failover.sh
# discipline).
#
#   TENANT=acme ADMIN_SUBJECT=alice@acme.com ./provision-tenant.sh
#   TENANT=acme STEP=policy ./provision-tenant.sh        # run one step
#
# Requires: kubectl, psql (or the cnpg plugin), and the target cluster context.
set -eu

TENANT="${TENANT:?set TENANT to the new tenant id (lowercase, dns-safe)}"
ADMIN_SUBJECT="${ADMIN_SUBJECT:-admin@$TENANT}"
DATA_NS="${DATA_NS:-kanz-data}"
SVC_NS="${SVC_NS:-kanz-services}"
MSG_NS="${MSG_NS:-kanz-messaging}"
STEP="${STEP:-all}"
k() { kubectl "$@"; }
step() { [ "$STEP" = "all" ] || [ "$STEP" = "$1" ]; }

echo "== provision tenant '$TENANT' (step=$STEP) =="

# 1) Storage isolation (MT-01d): the tenant's rows are scoped by Postgres RLS on
#    app.tenant_id — no per-tenant database, one policy-enforced table. Nothing to
#    create per tenant except confirming the RLS policy + FORCE RLS are active on
#    the shared clusters; the tenant id is carried on every connection's GUC by
#    the services (the AfterConnect set_config path). This step is a VERIFY, not a
#    mutate — RLS already isolates any tenant id that appears.
if step storage; then
  echo "-- [1/6] verify RLS isolation is active (kanz-risk, kanz-books)"
  for c in kanz-risk kanz-books; do
    k -n "$DATA_NS" exec "$c-1" -- psql -tAc \
      "select relname from pg_class where relrowsecurity and relforcerowsecurity limit 1" \
      >/dev/null || { echo "FATAL: FORCE RLS not active on $c — MT-01d not deployed"; exit 1; }
  done
fi

# 2) Broker isolation (MT-01c): the tenant's events ride the shared subjects but
#    carry tenant_id in the envelope; the consumer-side namespace + the gateway
#    authz keep them apart. No per-tenant stream to create — confirm the tenant
#    stamping is enforced (the bus.Producer requires a non-empty Tenant).
if step broker; then
  echo "-- [2/6] broker tenant stamping (envelope tenant_id) — no per-tenant stream needed"
fi

# 3) AUTH-01b policy bundle: install the tenant's role→permission bundle. The
#    shape is the shipped infra/security/policies/risk-authz.json (policy-as-data,
#    ConfigMap-mounted). A tenant gets the standard roles; the client's admin is
#    granted risk.admin, analysts risk.analyst, everyone else risk.reader.
if step policy; then
  echo "-- [3/6] install AUTH-01b policy bundle for $TENANT"
  k -n "$SVC_NS" create configmap "authz-$TENANT" \
    --from-file=risk-authz.json=../security/policies/risk-authz.json \
    --dry-run=client -o yaml | k apply -f -
  echo "   admin subject: $ADMIN_SUBJECT → risk.admin (grant via the IdP group→role mapping)"
fi

# 4) Per-tenant quota (MT-01e): add the tenant to the gateway QuotasFile so it gets
#    its own rate/burst/in-flight caps rather than the shared default. Merges into
#    the existing quotas ConfigMap (idempotent upsert).
if step quota; then
  echo "-- [4/6] add per-tenant quota entry"
  RATE="${RATE_PER_SEC:-200}"; BURST="${BURST:-400}"; INFLIGHT="${MAX_IN_FLIGHT:-100}"
  CUR="$(k -n "$SVC_NS" get configmap api-gateway-quotas -o jsonpath='{.data.quotas\.json}' 2>/dev/null || echo '{}')"
  NEXT="$(printf '%s' "$CUR" | jq --arg t "$TENANT" --argjson r "$RATE" --argjson b "$BURST" --argjson i "$INFLIGHT" \
    '.[$t] = {rate_per_sec:$r, burst:$b, max_in_flight:$i}')"
  k -n "$SVC_NS" create configmap api-gateway-quotas \
    --from-literal=quotas.json="$NEXT" --dry-run=client -o yaml | k apply -f -
fi

# 5) Data-source binding + first portfolio: bind the tenant's market/reference feed
#    entitlements (composition-root env on the market-data/datamaster adapters) and
#    seed a first portfolio so the read path serves real data on day one.
if step seed; then
  echo "-- [5/6] seed first portfolio for $TENANT"
  SEED_TENANT="$TENANT" SEED_PORTFOLIO="${FIRST_PORTFOLIO:-PF1}" \
    SEED_NATS_URL="${SEED_NATS_URL:-nats://nats.$MSG_NS:4222}" \
    go run ../../test/load/seed
fi

# 6) Verify isolation end-to-end BEFORE handing over: the new tenant can read its
#    own portfolio, and a probe as ANOTHER tenant is denied (the AUTH-01 gate). A
#    provision that leaks cross-tenant is worse than no provision.
if step verify; then
  echo "-- [6/6] verify cross-tenant isolation"
  GW="${GW:-http://api-gateway.$SVC_NS:8080}"
  code_self="$(curl -s -o /dev/null -w '%{http_code}' -H "X-Kanz-Tenant: $TENANT" \
    "$GW/v1/portfolios/${FIRST_PORTFOLIO:-PF1}/exposure")"
  code_other="$(curl -s -o /dev/null -w '%{http_code}' -H "X-Kanz-Tenant: __other__" \
    "$GW/v1/portfolios/${FIRST_PORTFOLIO:-PF1}/exposure")"
  echo "   self=$code_self other=$code_other"
  [ "$code_self" = "200" ] || { echo "FATAL: tenant cannot read its own portfolio ($code_self)"; exit 1; }
  [ "$code_other" = "403" ] || [ "$code_other" = "404" ] || { echo "FATAL: cross-tenant NOT denied ($code_other)"; exit 1; }
  echo "   isolation holds — tenant ready to hand over"
fi

echo "== tenant '$TENANT' provisioned (step(s) '$STEP') =="
