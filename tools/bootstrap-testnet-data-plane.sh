#!/bin/sh

# One-shot Vault bootstrap for the Tokyo testnet data plane. Passwords are
# generated inside the Vault container, sent to Vault on stdin, and never
# printed, passed as external command arguments, or persisted outside Vault.

set -eu
umask 077

VAULT_BIN=${VAULT_BIN:-/bin/vault}
export VAULT_ADDR=https://127.0.0.1:8200
export VAULT_CACERT=/run/spire/certs/bundle.crt
export VAULT_TLS_SERVER_NAME=vault.vault.svc.cluster.local

if [ ! -r "$VAULT_CACERT" ]; then
    printf 'Vault trust bundle is not readable; refusing bootstrap.\n' >&2
    exit 1
fi

vault_token=''
secret_value=''
clear_secrets() {
    vault_token=''
    secret_value=''
    unset VAULT_TOKEN
}
trap clear_secrets EXIT HUP INT TERM
rm -f /vault/run/testnet-data-plane-secrets.complete

printf '%s' 'Vault operator token: ' >&2
IFS= read -r -s vault_token
printf '\n' >&2
if [ -z "$vault_token" ]; then
    printf 'The Vault token was empty; no changes were made.\n' >&2
    exit 1
fi
VAULT_TOKEN=$vault_token
export VAULT_TOKEN
vault_token=''
"$VAULT_BIN" token lookup >/dev/null

if ! "$VAULT_BIN" audit list -format=json | grep -q '"file/"'; then
    "$VAULT_BIN" audit enable -path=file file file_path=/vault/data/audit.log
fi
if ! "$VAULT_BIN" secrets list -format=json | grep -q '"kv/"'; then
    "$VAULT_BIN" secrets enable -path=kv -version=2 kv
fi
if ! "$VAULT_BIN" auth list -format=json | grep -q '"kubernetes/"'; then
    "$VAULT_BIN" auth enable -path=kubernetes kubernetes
fi
"$VAULT_BIN" write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc:443 >/dev/null

write_field() {
    path=$1
    key=$2
    value=$3
    if "$VAULT_BIN" kv metadata get "kv/${path}" >/dev/null 2>&1; then
        printf '%s' "$value" | "$VAULT_BIN" kv patch "kv/${path}" "${key}=-" >/dev/null
    else
        printf '%s' "$value" | "$VAULT_BIN" kv put "kv/${path}" "${key}=-" >/dev/null
    fi
}

random_hex() {
    od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
}

write_read_policy() {
    name=$1
    path=$2
    namespace=$3
    service_account=$4
    cat <<EOF | "$VAULT_BIN" policy write "${name}-read" - >/dev/null
path "kv/data/${path}" {
  capabilities = ["read"]
}
path "kv/metadata/${path}" {
  capabilities = ["read"]
}
EOF
    "$VAULT_BIN" write "auth/kubernetes/role/${name}" \
        "bound_service_account_names=${service_account}" \
        "bound_service_account_namespaces=${namespace}" \
        audience=vault \
        "token_policies=${name}-read" \
        token_no_default_policy=true \
        token_type=service \
        token_ttl=5m \
        token_max_ttl=15m >/dev/null
}

write_read_policy postgres-provisioner kanz/testnet/postgres kanz-data postgres-provisioner
for service in accounting audit identity oms regulatory risk-engine venue-binance venue-okx; do
    write_read_policy "$service" "kanz/${service}" kanz-services "$service"
done
write_read_policy redis kanz/redis kanz-messaging redis
write_read_policy api-gateway kanz/api-gateway kanz-services api-gateway

for mapping in \
    'accounting:accounting:accounting:kanz-books-rw' \
    'audit:audit:audit:kanz-books-rw' \
    'identity:identity:identity:kanz-identity-rw' \
    'oms:oms:oms:kanz-orders-rw' \
    'regulatory:regulatory:regulatory:kanz-compliance-rw' \
    'risk-engine:risk_engine:risk_engine:kanz-risk-rw' \
    'venue-binance:venue_binance:venue_binance:kanz-orders-rw' \
    'venue-okx:venue_okx:venue_okx:kanz-orders-rw'; do
    old_ifs=$IFS
    IFS=:
    set -- $mapping
    IFS=$old_ifs
    service=$1
    database=$2
    stem=$3
    host=$4

    app_password=$(random_hex)
    migrate_password=$(random_hex)
    write_field kanz/testnet/postgres "${stem}_app" "$app_password"
    write_field kanz/testnet/postgres "${stem}_migrate" "$migrate_password"
    write_field "kanz/${service}" dsn "postgresql://${stem}_app:${app_password}@${host}.kanz-data.svc:5432/${database}?sslmode=require"
    write_field "kanz/${service}" migrate_dsn "postgresql://${stem}_migrate:${migrate_password}@${host}.kanz-data.svc:5432/${database}?sslmode=require"
    app_password=''
    migrate_password=''
done

redis_password=$(random_hex)
write_field kanz/redis password "$redis_password"
write_field kanz/api-gateway redis_dsn "redis://:${redis_password}@redis.kanz-messaging.svc:6379/0"
redis_password=''

for path in kanz/testnet/postgres kanz/redis kanz/api-gateway kanz/accounting kanz/audit kanz/identity kanz/oms kanz/regulatory kanz/risk-engine kanz/venue-binance kanz/venue-okx; do
    "$VAULT_BIN" kv metadata get "kv/${path}" >/dev/null
done
: > /vault/run/testnet-data-plane-secrets.complete
printf '%s\n' 'BOOTSTRAP_COMPLETE'
printf '%s\n' 'Generated Postgres/Redis credentials, least-privilege Kubernetes auth roles, and service DSNs are present in Vault.'
printf '%s\n' 'No credential value left the Vault container.'
