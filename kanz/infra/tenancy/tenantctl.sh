#!/usr/bin/env bash
# MT-01f: tenant lifecycle automation. ONE workflow that onboards/offboards a
# tenant across every isolation boundary the MT epic built, in dependency order:
#
#   1. identity  — k8s namespace + ServiceAccounts ⇒ SPIRE SVIDs (SEC-01a)
#   2. broker    — NATS account (MT-01c) + Kafka prefixed topics & ACLs (MT-01c)
#   3. state     — Postgres per-tenant login role for RLS (MT-01d)
#   4. quota     — gateway per-tenant budget entry (MT-01e)
#
# Identity is first because the SVID is the credential the broker (NATS account
# mapping, Kafka ACL principal) and DB role all key on. The schema registry is
# intentionally NOT provisioned — it is platform-global (MT-01d). Every step is
# idempotent, so a partial run re-runs cleanly. Offboard reverses the order.
#
# A tenant is defined by one env file (see tenant.example.env); source it, then:
#   ./tenantctl.sh onboard   |   ./tenantctl.sh offboard
#
# --- ONBOARD-M3 Task 1: preflight, and the rule that a manual step never lies -
# Before ONBOARD-M3 this script printed "tenant X onboarded" after every run,
# even when the NATS account was never minted (no nsc/NATS_OPERATOR — silently
# fell back to a "static mode" log line) or the Postgres role was never created
# (no ADMIN_DATABASE_URL — silently skipped). Under SEC-M3 the production
# broker maps a client's SVID to a NATS account via verify_and_map: a tenant
# with no account authenticates into NO account and cannot publish a single
# event. Because provision-tenant.sh (the client golden path) COMPOSES this
# script, that lie used to propagate straight into a paying client's hands.
#
# preflight() now runs FIRST — before onboard_identity/onboard_nats/... or
# offboard_quota/offboard_db/... and before any kubectl/psql/nsc call — and
# collects EVERY missing prerequisite in one pass, so a fix-one-rerun-find-the-
# next-one loop never happens:
#   - kubectl and jq on PATH: required unconditionally (both onboard and
#     offboard touch the cluster and the quota ConfigMap; there is no manual
#     escape for these two, because there is no per-step way around them).
#   - nsc on PATH + NATS_OPERATOR set: required for the NATS step, UNLESS the
#     operator declares TENANTCTL_MANUAL_NATS=true (they are adding/removing
#     the account in nats/tenancy.yaml by hand — see that file's own header
#     for the static/manual path it documents).
#   - ADMIN_DATABASE_URL: required for the DB step, UNLESS the operator
#     declares TENANTCTL_MANUAL_DB=true (they are provisioning/revoking the
#     Postgres role by hand). TENANT_DB_PASSWORD is required in addition,
#     but ONLY when ADMIN_DATABASE_URL is set on an onboard run — i.e. only
#     when the DB step is actually about to run psql for real. Once
#     ADMIN_DATABASE_URL is set the manual escape no longer applies to the
#     password: that combination means "run it for real," and a real CREATE
#     ROLE needs a real password.
#
# TENANTCTL_MANUAL_NATS=true / TENANTCTL_MANUAL_DB=true are the ONLY way to
# proceed without the tooling above, and each must be set explicitly and
# affirmatively — the same DATAMASTER_ALLOW_SIM stance used elsewhere in this
# repo: absence of configuration means degrade-with-a-name or REFUSE, never
# fabricate. Without the matching flag, a missing prerequisite is always a
# REFUSAL, never a silent skip.
#
# Exit codes:
#   0 = fully onboarded/offboarded — every step ran for real, no manual steps.
#   2 = REFUSED — a required prerequisite is missing and no manual escape was
#       declared (also used for a bad/missing subcommand). Nothing was
#       touched: preflight is the first thing each case branch calls, and it
#       only shells out to `command -v`, never to kubectl/psql/nsc.
#   3 = PARTIAL — at least one step was left for a human via
#       TENANTCTL_MANUAL_NATS=true / TENANTCTL_MANUAL_DB=true. The tenant is
#       NOT considered onboarded (or offboarded): the success line is never
#       printed, a PARTIAL summary names exactly what a human must still do,
#       and the exit is non-zero so provision-tenant.sh's `set -eu`
#       composition aborts rather than seeding a portfolio onto broker/DB
#       infrastructure that does not exist yet.
set -euo pipefail

: "${TENANT:?set TENANT (e.g. acme)}"
: "${TENANT_SAS:=risk-engine api-gateway}"            # ServiceAccounts to mint
: "${TRUST_DOMAIN:=kanz.internal}"
: "${MSG_NS:=kanz-messaging}"                         # NATS/Kafka namespace
: "${RATE_PER_SEC:=50}" "${BURST:=100}" "${MAX_IN_FLIGHT:=64}"
: "${KAFKA_JOB_TIMEOUT:=300}"                         # seconds, onboard_kafka Job wait

NS="tenant-${TENANT}"
principal() { echo "spiffe://${TRUST_DOMAIN}/ns/${NS}/sa/$1"; }
log() { echo ">> [$1] $2"; }

# MANUAL_STEPS accumulates a human-readable line per step that was left for an
# operator to do by hand (TENANTCTL_MANUAL_NATS/TENANTCTL_MANUAL_DB). finish()
# checks this at the very end: non-empty means PARTIAL, never success.
MANUAL_STEPS=()

# --- preflight: refuse before mutating, once, naming everything missing ----
preflight() {
  local mode="$1"
  local missing=()

  command -v kubectl >/dev/null 2>&1 || missing+=("kubectl not on PATH")
  command -v jq      >/dev/null 2>&1 || missing+=("jq not on PATH (required by the quota step)")

  if ! command -v nsc >/dev/null 2>&1 || [ -z "${NATS_OPERATOR:-}" ]; then
    if [ "${TENANTCTL_MANUAL_NATS:-false}" != "true" ]; then
      missing+=("nsc on PATH + NATS_OPERATOR set (or export TENANTCTL_MANUAL_NATS=true to add/remove the NATS account by hand)")
    fi
  fi

  if [ -z "${ADMIN_DATABASE_URL:-}" ]; then
    if [ "${TENANTCTL_MANUAL_DB:-false}" != "true" ]; then
      missing+=("ADMIN_DATABASE_URL (or export TENANTCTL_MANUAL_DB=true to provision/revoke the Postgres role by hand)")
    fi
  elif [ "${mode}" = "onboard" ] && [ -z "${TENANT_DB_PASSWORD:-}" ]; then
    # ADMIN_DATABASE_URL is set, so the DB step WILL run psql for real on this
    # onboard — the manual escape does not apply here; a real CREATE ROLE
    # needs a real password.
    missing+=("TENANT_DB_PASSWORD (required: ADMIN_DATABASE_URL is set, so the Postgres role step will run for real)")
  fi

  if [ "${#missing[@]}" -gt 0 ]; then
    echo "REFUSED: cannot ${mode} tenant '${TENANT}' — missing prerequisite(s):" >&2
    local m
    for m in "${missing[@]}"; do
      echo "  - ${m}" >&2
    done
    exit 2
  fi
}

# finish: print the real outcome. A manual step anywhere means this tenant is
# NOT onboarded/offboarded — print PARTIAL and fail the composing golden path,
# never the success line.
finish() {
  local mode="$1"
  if [ "${#MANUAL_STEPS[@]}" -gt 0 ]; then
    echo ">> [PARTIAL] tenant '${TENANT}' ${mode}: manual step(s) remain and must be completed by hand:" >&2
    local s
    for s in "${MANUAL_STEPS[@]}"; do
      echo "  - ${s}" >&2
    done
    echo ">> [PARTIAL] re-run without TENANTCTL_MANUAL_* once the step(s) above are done to get a real success" >&2
    exit 3
  fi
  log done "tenant ${TENANT} ${mode}ed"
}

# --- 1. identity ----------------------------------------------------------
onboard_identity() {
  log identity "namespace ${NS} (+SPIFFE) and ServiceAccounts: ${TENANT_SAS}"
  kubectl apply -f - <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: ${NS}
  labels:
    app.kubernetes.io/part-of: kanz
    kanz.internal/spiffe: enabled           # SEC-01a issues SVIDs to pods here
    kanz.internal/tenant: "${TENANT}"
YAML
  for sa in ${TENANT_SAS}; do
    kubectl -n "${NS}" create serviceaccount "${sa}" \
      --dry-run=client -o yaml | kubectl apply -f -
  done
}

offboard_identity() {
  log identity "deleting namespace ${NS} (cascades ServiceAccounts + SVID entries)"
  kubectl delete namespace "${NS}" --ignore-not-found
}

# --- 2. broker: NATS account + Kafka topics/ACLs --------------------------
# NATS account isolation (MT-01c): with the dynamic JWT account-resolver the
# account is minted + pushed with no server reload. With the static template,
# append the account to nats/tenancy.yaml keyed on the SVIDs below and reload.
# Static mode is a real, documented workflow (see nats/tenancy.yaml's header) —
# it is legitimate ONLY when declared explicitly via TENANTCTL_MANUAL_NATS=true
# (enforced by preflight); it is never a silent fallback, and it always counts
# as a manual step (recorded in MANUAL_STEPS) so finish() reports PARTIAL.
onboard_nats() {
  local users; users=$(for sa in ${TENANT_SAS}; do echo -n "$(principal "$sa") "; done)
  if ! command -v nsc >/dev/null 2>&1 || [ -z "${NATS_OPERATOR:-}" ]; then
    log nats "MANUAL (TENANTCTL_MANUAL_NATS=true): add account '${TENANT}' to nats/tenancy.yaml for users [${users}] then 'nats-server --signal reload'"
    MANUAL_STEPS+=("nats: account '${TENANT}' not minted — add it to nats/tenancy.yaml for users [${users}] and reload nats-server")
    return
  fi
  log nats "minting + pushing account ${TENANT} (resolver)"
  # `nsc add account` fails on a rerun because the account already exists —
  # that is the ONLY failure this step tolerates (idempotency). Anything else
  # (auth failure, unreachable operator store, disk full, ...) is a real
  # failure and must abort, not be swallowed by a blanket `|| true`: the old
  # `|| true` here let `edit`/`push` run next against an account that might
  # not exist at all.
  local add_out
  if ! add_out=$(nsc add account --name "${TENANT}" 2>&1); then
    if ! grep -qi 'already exist' <<<"${add_out}"; then
      log nats "FAILED to create account ${TENANT}: ${add_out}"
      return 1
    fi
    log nats "account ${TENANT} already exists — continuing"
  fi
  nsc edit account --name "${TENANT}" --js-mem-storage 64M --js-disk-storage 2G >/dev/null
  nsc push -a "${TENANT}"
}
offboard_nats() {
  if ! command -v nsc >/dev/null 2>&1 || [ -z "${NATS_OPERATOR:-}" ]; then
    log nats "MANUAL (TENANTCTL_MANUAL_NATS=true): remove account '${TENANT}' from nats/tenancy.yaml and reload nats-server"
    MANUAL_STEPS+=("nats: account '${TENANT}' not deleted — remove it from nats/tenancy.yaml and reload nats-server")
    return
  fi
  log nats "deleting account ${TENANT}"
  # Symmetric with onboard_nats: tolerate only "already gone" (a rerun/offboard
  # of an account that was never fully created), propagate everything else.
  local del_out
  if ! del_out=$(nsc delete account --name "${TENANT}" --revoke 2>&1); then
    if ! grep -Eqi 'not found|no such account|does not exist' <<<"${del_out}"; then
      log nats "FAILED to delete account ${TENANT}: ${del_out}"
      return 1
    fi
    log nats "account ${TENANT} already absent — continuing"
  fi
  nsc push -A
}

# wait_for_job bounds and OBSERVES a Job instead of firing-and-forgetting it —
# a `kubectl apply` with nobody watching the result is the same lie as a
# silent skip. It polls in slices rather than issuing one long
# `kubectl wait --for=condition=complete --timeout=<total>`, specifically so a
# Job that reaches `condition=failed` (backoffLimit exhausted) is caught within
# one slice instead of burning the entire timeout waiting for a `complete`
# condition it will never reach — the exact "fail fast, don't just wait it
# out" behavior called for over a single long wait-for-complete call.
wait_for_job() {
  local ns="$1" job="$2" total="${3:-${KAFKA_JOB_TIMEOUT}}"
  local slice=10 elapsed=0
  while [ "${elapsed}" -lt "${total}" ]; do
    if kubectl -n "${ns}" wait --for=condition=complete "job/${job}" --timeout="${slice}s" >/dev/null 2>&1; then
      return 0
    fi
    if kubectl -n "${ns}" wait --for=condition=failed "job/${job}" --timeout=1s >/dev/null 2>&1; then
      log kafka "job ${job} reported condition=failed"
      kubectl -n "${ns}" logs "job/${job}" --all-containers --tail=50 2>/dev/null || true
      return 1
    fi
    elapsed=$(( elapsed + slice ))
  done
  log kafka "job ${job} did not reach condition=complete within ${total}s"
  kubectl -n "${ns}" logs "job/${job}" --all-containers --tail=50 2>/dev/null || true
  return 1
}

# Kafka prefixed topics + PREFIXED ACLs (MT-01c) — run the kafka-tenant Job once
# per tenant ServiceAccount principal, reusing the kafka-tenant ConfigMap script.
# Each Job is waited on and its failure surfaced — an apply with nobody
# checking the outcome is a fire-and-forget lie, not a completed step.
onboard_kafka() {
  for sa in ${TENANT_SAS}; do
    log kafka "prefixed topics + ACLs for $(principal "$sa")"
    local job="kafka-tenant-${TENANT}-${sa}"
    kubectl -n "${MSG_NS}" delete job "${job}" --ignore-not-found
    kubectl apply -f - <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job}
  namespace: ${MSG_NS}
spec:
  backoffLimit: 4
  template:
    spec:
      restartPolicy: OnFailure
      serviceAccountName: kafka-provisioner
      initContainers:
        - name: spiffe-helper-init
          image: ghcr.io/spiffe/spiffe-helper:0.9.0
          args: ["-config", "/etc/spiffe-helper/helper.conf", "-daemon-mode=false"]
          volumeMounts:
            - { name: spiffe-helper-config, mountPath: /etc/spiffe-helper, readOnly: true }
            - { name: kafka-certs, mountPath: /etc/kafka-certs }
            - { name: spiffe, mountPath: /run/spiffe, readOnly: true }
      containers:
        - name: tenant
          image: apache/kafka:3.9.0
          command: ["bash", "/scripts/tenant.sh"]
          env:
            - { name: KAFKA_BOOTSTRAP, value: "kafka:9094" }
            - { name: KAFKA_CMD_CONFIG, value: "/etc/kafka-client/client.properties" }
            - { name: TENANT, value: "${TENANT}" }
            - { name: PRINCIPAL, value: "User:$(principal "$sa")" }
          volumeMounts:
            - { name: scripts, mountPath: /scripts }
            - { name: client, mountPath: /etc/kafka-client, readOnly: true }
            - { name: kafka-certs, mountPath: /etc/kafka-certs, readOnly: true }
      volumes:
        - { name: scripts, configMap: { name: kafka-tenant } }
        - { name: client, configMap: { name: kafka-provisioner-client } }
        - { name: kafka-certs, emptyDir: { medium: Memory } }
        - { name: spiffe, csi: { driver: csi.spiffe.io, readOnly: true } }
        - { name: spiffe-helper-config, configMap: { name: kafka-spiffe-helper } }
YAML
    if ! wait_for_job "${MSG_NS}" "${job}"; then
      log kafka "FAILED: ${job} did not complete — aborting onboard"
      return 1
    fi
    log kafka "job ${job} completed"
  done
}
# Teardown Job: revoke each principal's PREFIXED ACLs FIRST (so no grant
# outlives the topics), then delete every {tenant}. topic.
offboard_kafka() {
  local removes=""
  for sa in ${TENANT_SAS}; do
    removes+="kafka-acls.sh \$BS \$CC --remove --force --allow-principal 'User:$(principal "$sa")' --operation Write --operation Read --operation Describe --operation Create --topic '${TENANT}.' --resource-pattern-type prefixed; "
    removes+="kafka-acls.sh \$BS \$CC --remove --force --allow-principal 'User:$(principal "$sa")' --operation Read --operation Describe --group '${TENANT}.' --resource-pattern-type prefixed; "
  done
  log kafka "revoking ACLs + deleting ${TENANT}.* topics"
  kubectl -n "${MSG_NS}" delete job "kafka-offboard-${TENANT}" --ignore-not-found
  kubectl apply -f - <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  name: kafka-offboard-${TENANT}
  namespace: ${MSG_NS}
spec:
  backoffLimit: 4
  template:
    spec:
      restartPolicy: OnFailure
      serviceAccountName: kafka-provisioner
      initContainers:
        - name: spiffe-helper-init
          image: ghcr.io/spiffe/spiffe-helper:0.9.0
          args: ["-config", "/etc/spiffe-helper/helper.conf", "-daemon-mode=false"]
          volumeMounts:
            - { name: spiffe-helper-config, mountPath: /etc/spiffe-helper, readOnly: true }
            - { name: kafka-certs, mountPath: /etc/kafka-certs }
            - { name: spiffe, mountPath: /run/spiffe, readOnly: true }
      containers:
        - name: offboard
          image: apache/kafka:3.9.0
          command: ["bash", "-c"]
          args:
            - |
              set -euo pipefail
              BIN=/opt/kafka/bin; export PATH=\$BIN:\$PATH
              BS="--bootstrap-server kafka:9094"
              CC="--command-config /etc/kafka-client/client.properties"
              ${removes}
              for t in \$(kafka-topics.sh \$BS \$CC --list | grep -E '^${TENANT}\.' || true); do
                kafka-topics.sh \$BS \$CC --delete --topic "\$t"
              done
          volumeMounts:
            - { name: client, mountPath: /etc/kafka-client, readOnly: true }
            - { name: kafka-certs, mountPath: /etc/kafka-certs, readOnly: true }
      volumes:
        - { name: client, configMap: { name: kafka-provisioner-client } }
        - { name: kafka-certs, emptyDir: { medium: Memory } }
        - { name: spiffe, csi: { driver: csi.spiffe.io, readOnly: true } }
        - { name: spiffe-helper-config, configMap: { name: kafka-spiffe-helper } }
YAML
}

# --- 3. state: Postgres role for RLS (MT-01d) -----------------------------
# RLS policies already exist (0002); a tenant needs only a non-superuser login
# role granted on the state tables. The engine for this tenant connects as the
# role and sets app.tenant_id from cfg.Tenant — FORCE RLS then scopes it.
# The manual path (no ADMIN_DATABASE_URL) is a real workflow, but it is
# legitimate ONLY when declared via TENANTCTL_MANUAL_DB=true (enforced by
# preflight) — it always counts as a manual step, never a silent skip.
onboard_db() {
  if [ -z "${ADMIN_DATABASE_URL:-}" ]; then
    log db "MANUAL (TENANTCTL_MANUAL_DB=true): create login role kanz_tenant_${TENANT} yourself and GRANT SELECT, INSERT, UPDATE, DELETE ON portfolios, positions, applied_keys TO kanz_tenant_${TENANT}"
    MANUAL_STEPS+=("db: Postgres login role kanz_tenant_${TENANT} not created — create + grant it by hand before this tenant can read/write state")
    return
  fi
  log db "login role kanz_tenant_${TENANT}"
  psql "${ADMIN_DATABASE_URL}" -v ON_ERROR_STOP=1 <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kanz_tenant_${TENANT}') THEN
    CREATE ROLE kanz_tenant_${TENANT} LOGIN PASSWORD '${TENANT_DB_PASSWORD:?set TENANT_DB_PASSWORD}';
  END IF;
END \$\$;
GRANT SELECT, INSERT, UPDATE, DELETE ON portfolios, positions, applied_keys TO kanz_tenant_${TENANT};
SQL
}
offboard_db() {
  if [ -z "${ADMIN_DATABASE_URL:-}" ]; then
    log db "MANUAL (TENANTCTL_MANUAL_DB=true): revoke + drop role kanz_tenant_${TENANT} yourself"
    MANUAL_STEPS+=("db: Postgres login role kanz_tenant_${TENANT} not revoked — REVOKE + DROP ROLE by hand")
    return
  fi
  log db "revoking role kanz_tenant_${TENANT} (tenant rows kept for audit unless PURGE_ROWS=1)"
  if [ "${PURGE_ROWS:-0}" = "1" ]; then
    psql "${ADMIN_DATABASE_URL}" -v ON_ERROR_STOP=1 -c \
      "SET app.tenant_id = '${TENANT}'; DELETE FROM portfolios;"  # cascades positions/keys
  fi
  psql "${ADMIN_DATABASE_URL}" -v ON_ERROR_STOP=1 <<SQL
REVOKE ALL ON portfolios, positions, applied_keys FROM kanz_tenant_${TENANT};
DROP ROLE IF EXISTS kanz_tenant_${TENANT};
SQL
}

# --- 4. quota: gateway per-tenant budget (MT-01e) -------------------------
# Merge/remove this tenant's entry in the api-gateway quota overrides ConfigMap
# (api-gateway loads it via API_GATEWAY_QUOTAS_FILE).
QUOTA_CM=${QUOTA_CM:-api-gateway-quotas}
SVC_NS=${SVC_NS:-kanz-services}
read_quotas() { kubectl -n "${SVC_NS}" get cm "${QUOTA_CM}" -o jsonpath='{.data.quotas\.json}' 2>/dev/null || echo '{}'; }
write_quotas() { kubectl -n "${SVC_NS}" create cm "${QUOTA_CM}" --from-file=quotas.json=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -; }
onboard_quota() {
  log quota "budget ${TENANT}: ${RATE_PER_SEC}/s burst ${BURST} maxInFlight ${MAX_IN_FLIGHT}"
  read_quotas | jq --arg t "${TENANT}" \
    --argjson r "${RATE_PER_SEC}" --argjson b "${BURST}" --argjson m "${MAX_IN_FLIGHT}" \
    '.[$t] = {rate_per_sec:$r, burst:$b, max_in_flight:$m}' | write_quotas
}
offboard_quota() {
  log quota "removing budget ${TENANT}"
  read_quotas | jq --arg t "${TENANT}" 'del(.[$t])' | write_quotas
}

case "${1:-}" in
  onboard)  preflight onboard
            onboard_identity; onboard_nats; onboard_kafka; onboard_db; onboard_quota
            finish onboard ;;
  offboard) preflight offboard
            offboard_quota; offboard_db; offboard_kafka; offboard_nats; offboard_identity
            finish offboard ;;
  *) echo "usage: TENANT=<name> $0 {onboard|offboard}" >&2; exit 2 ;;
esac
