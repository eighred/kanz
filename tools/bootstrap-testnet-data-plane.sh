#!/bin/sh

# One-shot Vault bootstrap for the Tokyo testnet data plane. Passwords are
# generated inside the Vault container, sent to Vault on stdin, and never
# printed, passed as external command arguments, or persisted outside Vault.

set -eu
umask 077

VAULT_BIN=${VAULT_BIN:-/bin/vault}
VAULT_ADDR=${VAULT_ADDR:-https://127.0.0.1:8200}
VAULT_CACERT=${VAULT_CACERT:-/run/spire/certs/bundle.crt}
VAULT_TLS_SERVER_NAME=${VAULT_TLS_SERVER_NAME:-vault.vault.svc.cluster.local}
COMPLETION_MARKER=${COMPLETION_MARKER:-/vault/run/testnet-data-plane-secrets.complete}
export VAULT_ADDR VAULT_CACERT VAULT_TLS_SERVER_NAME

if [ ! -r "$VAULT_CACERT" ]; then
    printf 'Vault trust bundle is not readable; refusing bootstrap.\n' >&2
    exit 1
fi

vault_token=''
secret_value=''
exported_key_json=''
terminal_state=''
restore_terminal() {
    if [ -n "$terminal_state" ]; then
        stty "$terminal_state"
        terminal_state=''
    fi
}
clear_secrets() {
    restore_terminal
    vault_token=''
    secret_value=''
    exported_key_json=''
    unset VAULT_TOKEN
}
trap clear_secrets EXIT HUP INT TERM
rm -f "$COMPLETION_MARKER"

printf '%s' 'Vault operator token: ' >&2
if [ -t 0 ]; then
    terminal_state=$(stty -g)
    stty -echo
fi
IFS= read -r vault_token
restore_terminal
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

field_exists() {
    "$VAULT_BIN" kv get -field="$2" "kv/$1" >/dev/null 2>&1
}

# Print complete or absent. A partial credential group is neither: generating
# only its missing half could bind a new DSN to an old database password.
field_group_state() {
    label=$1
    shift
    present=0
    total=$#
    for field in "$@"; do
        path=${field%%:*}
        key=${field#*:}
        if field_exists "$path" "$key"; then
            present=$((present + 1))
        fi
    done
    if [ "$present" -eq 0 ]; then
        printf '%s\n' absent
    elif [ "$present" -eq "$total" ]; then
        printf '%s\n' complete
    else
        printf 'Credential group %s is partial (%s/%s fields); refusing to overwrite it.\n' "$label" "$present" "$total" >&2
        return 1
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

    state=$(field_group_state "$service database" \
        "kanz/testnet/postgres:${stem}_app" \
        "kanz/testnet/postgres:${stem}_migrate" \
        "kanz/${service}:dsn" \
        "kanz/${service}:migrate_dsn")
    if [ "$state" = absent ]; then
        app_password=$(random_hex)
        migrate_password=$(random_hex)
        write_field kanz/testnet/postgres "${stem}_app" "$app_password"
        write_field kanz/testnet/postgres "${stem}_migrate" "$migrate_password"
        write_field "kanz/${service}" dsn "postgresql://${stem}_app:${app_password}@${host}.kanz-data.svc:5432/${database}?sslmode=require"
        write_field "kanz/${service}" migrate_dsn "postgresql://${stem}_migrate:${migrate_password}@${host}.kanz-data.svc:5432/${database}?sslmode=require"
        app_password=''
        migrate_password=''
    fi
done

redis_state=$(field_group_state 'Redis' 'kanz/redis:password' 'kanz/api-gateway:redis_dsn')
if [ "$redis_state" = absent ]; then
    redis_password=$(random_hex)
    write_field kanz/redis password "$redis_password"
    write_field kanz/api-gateway redis_dsn "redis://:${redis_password}@redis.kanz-messaging.svc:6379/0"
    redis_password=''
fi

if ! field_exists kanz/api-gateway signing_secret; then
    secret_value=$(random_hex)
    write_field kanz/api-gateway signing_secret "$secret_value"
    secret_value=''
fi

# Identity needs a durable P-256 PEM. Generate it inside Vault Transit, stream
# the exported version directly into KV, then remove the temporary engine. The
# private key never crosses kubectl/SSM and never reaches a persistent file.
if ! field_exists kanz/identity signing_key_pem; then
    if ! "$VAULT_BIN" secrets list -format=json | grep -q '"bootstrap-transit/"'; then
        "$VAULT_BIN" secrets enable -path=bootstrap-transit transit >/dev/null
    fi
    if ! "$VAULT_BIN" read bootstrap-transit/keys/identity-token >/dev/null 2>&1; then
        "$VAULT_BIN" write bootstrap-transit/keys/identity-token type=ecdsa-p256 exportable=true >/dev/null
    fi
    exported_key_json=$("$VAULT_BIN" read -format=json bootstrap-transit/export/signing-key/identity-token/1)
    secret_value=$(printf '%s\n' "$exported_key_json" | sed -n \
        -e 's/^[[:space:]]*"1":[[:space:]]*"\(.*\)",[[:space:]]*$/\1/p' \
        -e 's/^[[:space:]]*"1":[[:space:]]*"\(.*\)"[[:space:]]*$/\1/p' | sed 's/\\n/\
/g')
    exported_key_json=''
    case "$secret_value" in
        '-----BEGIN EC PRIVATE KEY-----'*|'-----BEGIN PRIVATE KEY-----'*) ;;
        *) printf '%s\n' 'Vault did not export a valid P-256 PEM; refusing to write it.' >&2; exit 1 ;;
    esac
    write_field kanz/identity signing_key_pem "$secret_value"
    secret_value=''
    "$VAULT_BIN" secrets disable bootstrap-transit >/dev/null
elif "$VAULT_BIN" secrets list -format=json | grep -q '"bootstrap-transit/"'; then
    "$VAULT_BIN" secrets disable bootstrap-transit >/dev/null
fi

for path in kanz/testnet/postgres kanz/redis kanz/api-gateway kanz/accounting kanz/audit kanz/identity kanz/oms kanz/regulatory kanz/risk-engine kanz/venue-binance kanz/venue-okx; do
    "$VAULT_BIN" kv metadata get "kv/${path}" >/dev/null
done
"$VAULT_BIN" kv get -field=signing_secret kv/kanz/api-gateway >/dev/null
"$VAULT_BIN" kv get -field=signing_key_pem kv/kanz/identity >/dev/null
: > "$COMPLETION_MARKER"
printf '%s\n' 'BOOTSTRAP_COMPLETE'
printf '%s\n' 'Postgres, Redis, gateway-signing, and identity-signing material and least-privilege Kubernetes auth roles are present in Vault.'
printf '%s\n' 'No credential value left the Vault container.'
