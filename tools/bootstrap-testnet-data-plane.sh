#!/bin/sh

# One-shot Vault bootstrap for the Tokyo testnet data plane. Passwords are
# generated inside the Vault container, sent to Vault on stdin, and never
# printed, passed as external command arguments, or persisted outside Vault.

set -eu
umask 077
bootstrap_mode=${1:-base}
case "$bootstrap_mode" in base|tv-sync) ;; *) printf '%s\n' 'unsupported bootstrap mode' >&2; exit 2 ;; esac

VAULT_BIN=${VAULT_BIN:-/bin/vault}
VAULT_ADDR=${VAULT_ADDR:-https://127.0.0.1:8200}
VAULT_CACERT=${VAULT_CACERT:-/run/spire/certs/bundle.crt}
VAULT_TLS_SERVER_NAME=${VAULT_TLS_SERVER_NAME:-vault.vault.svc.cluster.local}
if [ "$bootstrap_mode" = tv-sync ]; then
    COMPLETION_MARKER=${COMPLETION_MARKER:-/vault/run/testnet-tv-sync-secrets.complete}
else
    COMPLETION_MARKER=${COMPLETION_MARKER:-/vault/run/testnet-data-plane-secrets.complete}
fi
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
    field_result=''
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

if [ "$bootstrap_mode" = tv-sync ]; then
    # This extension requires the established bootstrap baseline; it never
    # enables engines, rewrites auth config, or generates unrelated credentials.
    "$VAULT_BIN" audit list -format=json | grep -q '"file/"'
    "$VAULT_BIN" secrets list -format=json | grep -q '"kv/"'
    "$VAULT_BIN" auth list -format=json | grep -q '"kubernetes/"'
    "$VAULT_BIN" read auth/kubernetes/role/postgres-provisioner >/dev/null
else
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

fi

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
    # Pinned Vault CLI distinguishes absent field/path from transport or ACL
    # failure (v1.16.2 command/util.go and command/kv_get.go). Never echo values.
    if field_result=$("$VAULT_BIN" kv get -field="$2" "kv/$1" 2>&1); then
        field_result=''
        return 0
    fi
    if [ "$field_result" = "Field \"$2\" not present in secret" ] ||
       [ "$field_result" = "No value found at kv/data/$1" ]; then
        field_result=''
        return 1
    fi
    field_result=''
    printf '%s\n' 'Vault field lookup failed; absence is unverified and bootstrap is refused.' >&2
    exit 1
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

if [ "$bootstrap_mode" = base ]; then
write_read_policy postgres-provisioner kanz/testnet/postgres kanz-data postgres-provisioner
for service in accounting audit identity oms regulatory risk-engine venue-binance venue-okx; do
    write_read_policy "$service" "kanz/${service}" kanz-services "$service"
done
write_read_policy redis kanz/redis kanz-messaging redis
write_read_policy api-gateway kanz/api-gateway kanz-services api-gateway

fi

for mapping in \
    'accounting:accounting:accounting:kanz-books-rw' \
    'audit:audit:audit:kanz-books-rw' \
    'identity:identity:identity:kanz-identity-rw' \
    'oms:oms:oms:kanz-orders-rw' \
    'regulatory:regulatory:regulatory:kanz-compliance-rw' \
    'risk-engine:risk_engine:risk_engine:kanz-risk-rw' \
    'venue-binance:venue_binance:venue_binance:kanz-orders-rw' \
    'venue-okx:venue_okx:venue_okx:kanz-orders-rw' \
    'tv-sync:tv_sync:tv_sync:kanz-books-rw'; do
    old_ifs=$IFS
    IFS=:
    set -- $mapping
    IFS=$old_ifs
    service=$1
    if [ "$service" = tv-sync ] && [ "$bootstrap_mode" = base ]; then continue; fi
    if [ "$service" != tv-sync ] && [ "$bootstrap_mode" = tv-sync ]; then continue; fi
    database=$2
    stem=$3
    host=$4

    runtime_path="kanz/${service}"
    if [ "$service" = tv-sync ]; then
        runtime_path=kanz/tv-sync-runtime
        # A KV read returns the entire record. Narrowing the CSI projection is
        # insufficient if this ServiceAccount can read a migration DSN there.
        if field_exists "$runtime_path" migrate_dsn; then
            printf '%s\n' 'tv-sync runtime KV contains a migration credential; refusing bootstrap.' >&2
            exit 1
        fi
        state=$(field_group_state "$service database" \
            "kanz/testnet/postgres:${stem}_app" \
            "kanz/testnet/postgres:${stem}_migrate" \
            "${runtime_path}:dsn")
    else
        state=$(field_group_state "$service database" \
            "kanz/testnet/postgres:${stem}_app" \
            "kanz/testnet/postgres:${stem}_migrate" \
            "${runtime_path}:dsn" \
            "${runtime_path}:migrate_dsn")
    fi
    if [ "$state" = absent ]; then
        app_password=$(random_hex)
        migrate_password=$(random_hex)
        write_field kanz/testnet/postgres "${stem}_app" "$app_password"
        write_field kanz/testnet/postgres "${stem}_migrate" "$migrate_password"
        write_field "$runtime_path" dsn "postgresql://${stem}_app:${app_password}@${host}.kanz-data.svc:5432/${database}?sslmode=require"
        if [ "$service" != tv-sync ]; then
            write_field "$runtime_path" migrate_dsn "postgresql://${stem}_migrate:${migrate_password}@${host}.kanz-data.svc:5432/${database}?sslmode=require"
        fi
        app_password=''
        migrate_password=''
    fi
done

# Publish runtime access only after the separate record and full credential group
# are qualified. Existing tokens require expiry/revocation before activation.
if [ "$bootstrap_mode" = tv-sync ]; then
    write_read_policy tv-sync kanz/tv-sync-runtime kanz-services tv-sync
    "$VAULT_BIN" kv metadata get kv/kanz/testnet/postgres >/dev/null
    "$VAULT_BIN" kv metadata get kv/kanz/tv-sync-runtime >/dev/null
    : > "$COMPLETION_MARKER"
    printf '%s\n' 'BOOTSTRAP_COMPLETE' 'tv-sync runtime-only record and role are qualified; no credential value left Vault.'
    exit 0
fi

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
