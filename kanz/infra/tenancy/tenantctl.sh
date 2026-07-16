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
set -euo pipefail

: "${TENANT:?set TENANT (e.g. acme)}"
: "${TENANT_SAS:=risk-engine api-gateway}"            # ServiceAccounts to mint
: "${TRUST_DOMAIN:=kanz.internal}"
: "${MSG_NS:=kanz-messaging}"                         # NATS/Kafka namespace
: "${RATE_PER_SEC:=50}" "${BURST:=100}" "${MAX_IN_FLIGHT:=64}"

NS="tenant-${TENANT}"
principal() { echo "spiffe://${TRUST_DOMAIN}/ns/${NS}/sa/$1"; }
log() { echo ">> [$1] $2"; }

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
onboard_nats() {
  local users; users=$(for sa in ${TENANT_SAS}; do echo -n "$(principal "$sa") "; done)
  if command -v nsc >/dev/null && [ -n "${NATS_OPERATOR:-}" ]; then
    log nats "minting + pushing account ${TENANT} (resolver)"
    nsc add account --name "${TENANT}" >/dev/null 2>&1 || true
    nsc edit account --name "${TENANT}" --js-mem-storage 64M --js-disk-storage 2G >/dev/null
    nsc push -a "${TENANT}"
  else
    log nats "static mode: add account '${TENANT}' to nats/tenancy.yaml for users [${users}] then 'nats-server --signal reload'"
  fi
}
offboard_nats() {
  if command -v nsc >/dev/null && [ -n "${NATS_OPERATOR:-}" ]; then
    log nats "deleting account ${TENANT}"
    nsc delete account --name "${TENANT}" --revoke >/dev/null 2>&1 || true
    nsc push -A
  else
    log nats "static mode: remove account '${TENANT}' from nats/tenancy.yaml and reload"
  fi
}

# Kafka prefixed topics + PREFIXED ACLs (MT-01c) — run the kafka-tenant Job once
# per tenant ServiceAccount principal, reusing the kafka-tenant ConfigMap script.
onboard_kafka() {
  for sa in ${TENANT_SAS}; do
    log kafka "prefixed topics + ACLs for $(principal "$sa")"
    kubectl -n "${MSG_NS}" delete job "kafka-tenant-${TENANT}-${sa}" --ignore-not-found
    kubectl apply -f - <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  name: kafka-tenant-${TENANT}-${sa}
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
onboard_db() {
  [ -z "${ADMIN_DATABASE_URL:-}" ] && { log db "skip (set ADMIN_DATABASE_URL to provision the role)"; return; }
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
  [ -z "${ADMIN_DATABASE_URL:-}" ] && { log db "skip"; return; }
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
  onboard)  onboard_identity; onboard_nats; onboard_kafka; onboard_db; onboard_quota
            log done "tenant ${TENANT} onboarded" ;;
  offboard) offboard_quota; offboard_db; offboard_kafka; offboard_nats; offboard_identity
            log done "tenant ${TENANT} offboarded" ;;
  *) echo "usage: TENANT=<name> $0 {onboard|offboard}" >&2; exit 2 ;;
esac
