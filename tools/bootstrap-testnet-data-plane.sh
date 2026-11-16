#!/bin/sh

# One-shot Vault bootstrap for the Tokyo testnet data plane. Passwords are
# generated inside the Vault container, sent to Vault on stdin, and never
# printed, passed as external command arguments, or persisted outside Vault.

set -eu
umask 077
bootstrap_mode=${1:-base}
case "$bootstrap_mode" in base|tv-sync|runtime-isolation-prepare|runtime-isolation-activate) ;; *) printf '%s\n' 'unsupported bootstrap mode' >&2; exit 2 ;; esac

VAULT_BIN=${VAULT_BIN:-/bin/vault}
VAULT_ADDR=${VAULT_ADDR:-https://127.0.0.1:8200}
VAULT_CACERT=${VAULT_CACERT:-/run/spire/certs/bundle.crt}
VAULT_TLS_SERVER_NAME=${VAULT_TLS_SERVER_NAME:-vault.vault.svc.cluster.local}
if [ "$bootstrap_mode" = tv-sync ]; then
    COMPLETION_MARKER=${COMPLETION_MARKER:-/vault/run/testnet-tv-sync-secrets.complete}
elif [ "$bootstrap_mode" = runtime-isolation-prepare ] || [ "$bootstrap_mode" = runtime-isolation-activate ]; then
    COMPLETION_MARKER=${COMPLETION_MARKER:-/vault/run/testnet-runtime-isolation.complete}
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
    copy_value=''
    copy_existing=''
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

if [ "$bootstrap_mode" != base ]; then
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
    if field_result=$("$VAULT_BIN" kv metadata get "kv/${path}" 2>&1); then
        field_result=''
        printf '%s' "$value" | "$VAULT_BIN" kv patch "kv/${path}" "${key}=-" >/dev/null
    elif [ "$field_result" = "No value found at kv/metadata/${path}" ]; then
        field_result=''
        printf '%s' "$value" | "$VAULT_BIN" kv put "kv/${path}" "${key}=-" >/dev/null
    else
        field_result=''
        printf '%s\n' 'Vault metadata lookup failed; refusing an unverified write.' >&2
        exit 1
    fi
    value=''
}

field_exists() {
    # Pinned Vault CLI distinguishes absent field/path from transport or ACL
    # failure (v1.21.4 command/util.go and command/kv_get.go). Never echo values.
    if [ "$#" = 3 ]; then
        if field_result=$("$VAULT_BIN" kv get -version="$3" -field="$2" "kv/$1" 2>&1); then
            field_result=''
            return 0
        fi
    elif field_result=$("$VAULT_BIN" kv get -field="$2" "kv/$1" 2>&1); then
        field_result=''
        return 0
    fi
    if [ "$field_result" = "Field \"$2\" not present in secret" ] ||
       { [ "$#" != 3 ] && [ "$field_result" = "No value found at kv/data/$1" ]; }; then
        field_result=''
        return 1
    fi
    field_result=''
    printf '%s\n' 'Vault field lookup failed; absence is unverified and bootstrap is refused.' >&2
    exit 1
}

# #1747 phase 1 covers these three callers only. Other mixed-record consumers
# remain on their existing contract; preparation does not activate a rollout.
isolation_state() {
    case "$1" in accounting|audit|identity) ;; *) printf '%s\n' legacy; return ;; esac
    if field_exists "kanz/$1-runtime" credential_isolation_state; then
        "$VAULT_BIN" kv get -field=credential_isolation_state "kv/kanz/$1-runtime"
    else
        printf '%s\n' legacy
    fi
}

runtime_secret_path() {
    runtime_state=$(isolation_state "$1")
    case "$runtime_state" in
        legacy|v1-prepared) printf 'kanz/%s\n' "$1" ;;
        v1-active) printf 'kanz/%s-runtime\n' "$1" ;;
        *) printf '%s\n' 'Runtime credential isolation is incomplete or unknown; refusing legacy fallback.' >&2; exit 1 ;;
    esac
}

record_version_count() {
    if version_result=$("$VAULT_BIN" read -field=current_version "kv/metadata/$1" 2>&1); then
        :
    elif [ "$2" = allow-new ] && [ "$version_result" = "No value found at kv/metadata/$1" ]; then
        version_result=0
    else
        version_result=''
        printf '%s\n' 'Credential history metadata is unavailable; refusing qualification.' >&2; exit 1
    fi
    # Ask the pinned CLI for one non-secret metadata field; never parse a
    # formatted secret record. Unknown or excessive history refuses.
    case "$version_result" in ''|*[!0-9]*) printf '%s\n' 'Credential history version is unknown.' >&2; exit 1 ;; esac
    if [ "$version_result" -gt 16 ] || { [ "$2" != allow-new ] && [ "$version_result" -lt 1 ]; }; then
        printf '%s\n' 'Credential history exceeds the bounded preparation contract; operator review is required.' >&2; exit 1
    fi
    printf '%s\n' "$version_result"
    version_result=''
}

reserve_history_writes() {
    budget_version=$(record_version_count "$1" allow-new)
    if [ $((budget_version + $2)) -gt 16 ]; then
        printf '%s\n' 'Credential history lacks the required write budget; refusing before preparation or policy publication.' >&2; exit 1
    fi
}

check_record_history() {
    history_path=$1
    shift
    history_versions=$(record_version_count "$history_path" existing)
    history_version=1
    while [ "$history_version" -le "$history_versions" ]; do
        for forbidden_field in "$@"; do
            if field_exists "$history_path" "$forbidden_field" "$history_version"; then
                printf '%s\n' 'Credential record history contains authority outside its role; refusing qualification.' >&2; exit 1
            fi
        done
        history_version=$((history_version + 1))
    done
}

copy_isolation_record() {
    copy_source=$1
    copy_target=$2
    shift 2
    copy_fields=''
    for copy_key in "$@"; do copy_fields="$copy_fields $copy_target:$copy_key"; done
    # All field names/paths are fixed by the selected component below. Expansion
    # supplies separate non-secret field descriptors, never secret values.
    # shellcheck disable=SC2086
    copy_state=$(field_group_state 'runtime isolation target' $copy_fields)
    for copy_key in "$@"; do
        if ! copy_value=$("$VAULT_BIN" kv get -field="$copy_key" "kv/$copy_source" 2>/dev/null); then
            printf '%s\n' 'Required source credential is unreadable.' >&2; exit 1
        fi
        test -n "$copy_value" || { printf '%s\n' 'Required source credential field is empty.' >&2; exit 1; }
        if [ "$copy_state" = absent ]; then
            write_field "$copy_target" "$copy_key" "$copy_value"
        else
            if ! copy_existing=$("$VAULT_BIN" kv get -field="$copy_key" "kv/$copy_target" 2>/dev/null); then
                printf '%s\n' 'Prepared credential is unreadable.' >&2; exit 1
            fi
            if [ "$copy_existing" != "$copy_value" ]; then
                copy_value=''; copy_existing=''
                printf '%s\n' 'Prepared credential differs from its approved source; refusing overwrite.' >&2; exit 1
            fi
        fi
        copy_value=''; copy_existing=''
    done
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

qualify_isolated_service() {
    isolation_service=$1
    isolation_runtime="kanz/$isolation_service-runtime"
    isolation_migration="kanz/$isolation_service-migration"
    if field_exists "$isolation_runtime" migrate_dsn; then
        printf '%s\n' 'Runtime KV contains a migration credential; refusing preparation.' >&2; exit 1
    fi
    check_record_history "$isolation_runtime" migrate_dsn
    check_record_history "$isolation_migration" dsn signing_key_pem
    if [ "$isolation_service" = identity ]; then
        isolation_group=$(field_group_state 'identity isolated credential' "$isolation_runtime:dsn" "$isolation_runtime:signing_key_pem" "$isolation_migration:migrate_dsn")
    else
        isolation_group=$(field_group_state 'isolated credential' "$isolation_runtime:dsn" "$isolation_migration:migrate_dsn")
    fi
    [ "$isolation_group" = complete ] || { printf '%s\n' 'Isolated credential group is absent; preparation is required.' >&2; exit 1; }
}

if [ "$bootstrap_mode" = runtime-isolation-prepare ]; then
    # Reserve every selected record's exact copy/marker writes before making
    # any write. A completed preparation must remain inside its own history
    # bound, including on ordinary idempotent reruns.
    for isolation_service in accounting audit identity; do
        preparation_state=$(isolation_state "$isolation_service")
        case "$preparation_state" in legacy|v1-prepared|v1-active) ;; *) printf '%s\n' 'Isolation cutover is incomplete; refusing preparation.' >&2; exit 1 ;; esac
        preparation_runtime="kanz/$isolation_service-runtime"
        if [ "$isolation_service" = identity ]; then
            preparation_group=$(field_group_state 'identity runtime target' "$preparation_runtime:dsn" "$preparation_runtime:signing_key_pem")
            preparation_writes=2
        else
            preparation_group=$(field_group_state 'runtime target' "$preparation_runtime:dsn")
            preparation_writes=1
        fi
        if [ "$preparation_group" = complete ]; then preparation_writes=0; fi
        if [ "$preparation_state" = legacy ]; then preparation_writes=$((preparation_writes + 1)); fi
        # A prepared record must still have room for both activation markers.
        # An already active record needs no future cutover reservation.
        if [ "$preparation_state" != v1-active ]; then preparation_writes=$((preparation_writes + 2)); fi
        reserve_history_writes "$preparation_runtime" "$preparation_writes"
        preparation_migration="kanz/$isolation_service-migration"
        preparation_group=$(field_group_state 'migration target' "$preparation_migration:migrate_dsn")
        preparation_writes=1
        if [ "$preparation_group" = complete ]; then preparation_writes=0; fi
        reserve_history_writes "$preparation_migration" "$preparation_writes"
    done
    for isolation_service in accounting audit identity; do
        isolation_current=$(isolation_state "$isolation_service")
        case "$isolation_current" in legacy|v1-prepared|v1-active) ;; *) printf '%s\n' 'Isolation cutover is incomplete; refusing preparation.' >&2; exit 1 ;; esac
        if [ "$isolation_service" = identity ]; then
            copy_isolation_record "kanz/$isolation_service" "kanz/$isolation_service-runtime" dsn signing_key_pem
        else
            copy_isolation_record "kanz/$isolation_service" "kanz/$isolation_service-runtime" dsn
        fi
        copy_isolation_record "kanz/$isolation_service" "kanz/$isolation_service-migration" migrate_dsn
        qualify_isolated_service "$isolation_service"
        if [ "$isolation_current" = legacy ]; then
            write_field "$isolation_runtime" credential_isolation_state v1-prepared
        fi
        # New, unused migration identity only. Existing runtime policies and
        # ServiceAccounts remain untouched until the separate approved cutover.
        write_read_policy "$isolation_service-migrate" "$isolation_migration" kanz-services "$isolation_service-migrate"
    done
    : > "$COMPLETION_MARKER"
    printf '%s\n' 'BOOTSTRAP_COMPLETE' 'Phase-1 records and independent migration roles prepared; runtime access and workloads were not activated.'
    exit 0
fi

if [ "$bootstrap_mode" = runtime-isolation-activate ]; then
    # The token authenticates an operator; this explicit mode is not permission
    # to deploy, rotate credentials or waive migration/continuity proof. The
    # caller must first perform the reviewed runbook's isolated/main and operator
    # acceptance. A partial policy write leaves an explicit refusing state.
    [ "${RUNTIME_ISOLATION_CUTOVER_APPROVED:-}" = yes ] || { printf '%s\n' 'Runtime credential cutover requires separate operator authorization and prerequisite proof.' >&2; exit 1; }
    # Qualify and reserve both state markers across the whole selected set
    # before publishing the first policy. Do not finish at version17/18 and
    # leave the next ordinary bootstrap unable to qualify its own result.
    for isolation_service in accounting audit identity; do
        isolation_current=$(isolation_state "$isolation_service")
        case "$isolation_current" in v1-prepared|v1-activating|v1-active) ;; *) printf '%s\n' 'Runtime credential preparation is missing.' >&2; exit 1 ;; esac
        qualify_isolated_service "$isolation_service"
        reserve_history_writes "$isolation_runtime" 2
    done
    for isolation_service in accounting audit identity; do
        isolation_runtime="kanz/$isolation_service-runtime"
        write_field "$isolation_runtime" credential_isolation_state v1-activating
        write_read_policy "$isolation_service" "$isolation_runtime" kanz-services "$isolation_service"
        write_field "$isolation_runtime" credential_isolation_state v1-active
    done
    : > "$COMPLETION_MARKER"
    printf '%s\n' 'BOOTSTRAP_COMPLETE' 'Runtime policies use isolated records; deployed continuity, old tokens and credential revocation still require operator evidence.'
    exit 0
fi

if [ "$bootstrap_mode" = base ]; then
write_read_policy postgres-provisioner kanz/testnet/postgres kanz-data postgres-provisioner
for service in accounting audit identity oms regulatory risk-engine venue-binance venue-okx; do
    selected_runtime=$(runtime_secret_path "$service")
    if [ "$(isolation_state "$service")" = v1-active ]; then qualify_isolated_service "$service"; fi
    write_read_policy "$service" "$selected_runtime" kanz-services "$service"
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

    runtime_path=$(runtime_secret_path "$service")
    service_isolation=$(isolation_state "$service")
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
    elif [ "$service_isolation" = v1-active ]; then
        qualify_isolated_service "$service"
        state=$(field_group_state "$service database" \
            "kanz/testnet/postgres:${stem}_app" \
            "kanz/testnet/postgres:${stem}_migrate" \
            "${runtime_path}:dsn" \
            "kanz/${service}-migration:migrate_dsn")
        [ "$state" = complete ] || { printf '%s\n' 'Active isolated credential group is incomplete; refusing legacy fallback.' >&2; exit 1; }
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
identity_runtime_path=$(runtime_secret_path identity)
if ! field_exists "$identity_runtime_path" signing_key_pem; then
    if [ "$(isolation_state identity)" = v1-active ]; then
        printf '%s\n' 'Active identity signing material is missing; refusing key replacement.' >&2; exit 1
    fi
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
    write_field "$identity_runtime_path" signing_key_pem "$secret_value"
    secret_value=''
    "$VAULT_BIN" secrets disable bootstrap-transit >/dev/null
elif "$VAULT_BIN" secrets list -format=json | grep -q '"bootstrap-transit/"'; then
    "$VAULT_BIN" secrets disable bootstrap-transit >/dev/null
fi

for path in kanz/testnet/postgres kanz/redis kanz/api-gateway kanz/accounting kanz/audit kanz/identity kanz/oms kanz/regulatory kanz/risk-engine kanz/venue-binance kanz/venue-okx; do
    "$VAULT_BIN" kv metadata get "kv/${path}" >/dev/null
done
"$VAULT_BIN" kv get -field=signing_secret kv/kanz/api-gateway >/dev/null
"$VAULT_BIN" kv get -field=signing_key_pem "kv/$identity_runtime_path" >/dev/null
: > "$COMPLETION_MARKER"
printf '%s\n' 'BOOTSTRAP_COMPLETE'
printf '%s\n' 'Postgres, Redis, gateway-signing, and identity-signing material and least-privilege Kubernetes auth roles are present in Vault.'
printf '%s\n' 'No credential value left the Vault container.'
