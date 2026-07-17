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
#   - ADMIN_DATABASE_URL: required ONLY for `offboard` with PURGE_ROWS=1 — the
#     one step that opens a psql connection — UNLESS the operator declares
#     TENANTCTL_MANUAL_DB=true (they are deleting the tenant's rows by hand).
#     Onboard needs no DB credential: there is nothing to create per tenant.
#     Tenant isolation is FORCE RLS + the app.tenant_id GUC, so a tenant's rows
#     exist the moment its services write them (provision-tenant.sh's storage
#     step VERIFIES that isolation rather than provisioning anything).
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
#   1 = a genuine step failure (e.g. `set -euo pipefail` tripping on a failed
#       kubectl/psql/nsc call, or the Kafka provisioning/offboard Job failing
#       or timing out per wait_for_job). Nothing about this is a lie: the
#       step ran for real and reported failure, or never reached a terminal
#       state within KAFKA_JOB_TIMEOUT.
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
#
# Note on precedence: TENANTCTL_MANUAL_NATS=true (and TENANTCTL_MANUAL_DB=true)
# only ever relaxes preflight — it never forces the manual path. If nsc +
# NATS_OPERATOR (or ADMIN_DATABASE_URL, on a PURGE_ROWS=1 offboard) are actually
# present, onboard_nats / offboard_nats (and the purge step) still do the real
# thing and the flag is quietly ignored. Real-over-manual is intentional: a
# flag set out of habit after the tooling was fixed should not downgrade a
# real provisioning step to a manual one. It just means the flag alone is not
# proof anything is manual — check the tooling, not just the env var.
set -euo pipefail

: "${TENANT:?set TENANT (e.g. acme)}"
# TENANT is interpolated, unescaped, into two places that do not tolerate
# arbitrary characters: a SQL string literal in offboard_db's DELETE (a
# tenant name containing a single quote breaks out of the literal), and
# NS="tenant-${TENANT}" below, which becomes a Kubernetes namespace name —
# and a namespace name must already be a valid RFC1123 label (lowercase
# alphanumeric + '-'). So this charset is not a new restriction invented
# here; it is what the estate already requires of a tenant name the moment
# it touches Kubernetes, made explicit and enforced before that SQL literal
# or namespace is ever built. Refuse (exit 2 = REFUSED, matching every other
# preflight-style refusal in this script) rather than let a malformed
# TENANT reach either interpolation site.
case "${TENANT}" in
  *[!a-z0-9-]*) echo "REFUSED: TENANT '${TENANT}' must contain only lowercase letters, digits, and '-'" >&2; exit 2 ;;
esac
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
    # Known, accepted seam: this string-matches nsc's error PROSE, not a
    # stable error code. An nsc reword could break idempotent reruns — but
    # it would break them LOUDLY (falls into the FAILED branch below and
    # aborts), never silently, so it is acceptable without a stable
    # machine-readable alternative from nsc.
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
    # Same accepted seam as onboard_nats: matches nsc's error PROSE. A reword
    # fails loudly (FAILED branch, non-zero exit) rather than silently
    # treating a real deletion failure as "already absent", so this is a
    # known tradeoff, not an oversight.
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
  # Each iteration costs slice+1 seconds of real time (the complete-wait plus
  # the 1s failed-probe below), not just `slice` — account for both so
  # `elapsed` tracks wall clock and the loop actually bounds at ~`total`,
  # rather than the ~1.1x overrun you get by only counting `slice`.
  local slice=10 probe=1 elapsed=0
  while [ "${elapsed}" -lt "${total}" ]; do
    if kubectl -n "${ns}" wait --for=condition=complete "job/${job}" --timeout="${slice}s" >/dev/null 2>&1; then
      return 0
    fi
    if kubectl -n "${ns}" wait --for=condition=failed "job/${job}" --timeout="${probe}s" >/dev/null 2>&1; then
      log kafka "job ${job} reported condition=failed"
      kubectl -n "${ns}" logs "job/${job}" --all-containers --tail=50 2>/dev/null || true
      return 1
    fi
    elapsed=$(( elapsed + slice + probe ))
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
  local job="kafka-offboard-${TENANT}"
  if ! wait_for_job "${MSG_NS}" "${job}"; then
    log kafka "FAILED: ${job} did not complete — aborting offboard"
    return 1
  fi
  log kafka "job ${job} completed"
}

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
            onboard_identity; onboard_nats; onboard_kafka; onboard_quota
            finish onboard ;;
  offboard) preflight offboard
            offboard_quota; offboard_db; offboard_kafka; offboard_nats; offboard_identity
            finish offboard ;;
  *) echo "usage: TENANT=<name> $0 {onboard|offboard}" >&2; exit 2 ;;
esac
