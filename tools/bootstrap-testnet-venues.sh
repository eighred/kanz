#!/bin/sh

# Repository-owned Vault bootstrap and credential rotation for testnet adapters.
# Run interactively inside the vault container. Secret values are accepted only
# on stdin and are never written to this file, command arguments, or stdout.

set -eu
umask 077

mode=${1:-bootstrap}
case "$mode" in
    bootstrap|rotate|account-proof) ;;
    *)
        printf 'usage: %s [bootstrap|rotate|account-proof]\n' "$0" >&2
        exit 2
        ;;
esac

VAULT_BIN=${VAULT_BIN:-/bin/vault}
export VAULT_ADDR=${VAULT_ADDR:-https://127.0.0.1:8200}
export VAULT_CACERT=${VAULT_CACERT:-/run/spire/certs/bundle.crt}
export VAULT_TLS_SERVER_NAME=${VAULT_TLS_SERVER_NAME:-vault.vault.svc.cluster.local}

if [ ! -r "$VAULT_CACERT" ]; then
    printf 'Vault trust bundle is not readable at %s; refusing bootstrap.\n' "$VAULT_CACERT" >&2
    exit 1
fi

vault_token=''
binance_api_key=''
binance_api_secret=''
okx_api_key=''
okx_api_secret=''
okx_api_passphrase=''
binance_account_uid=''
okx_account_uid=''
secret_pipe_dir=''
secret_writer_pids=''
secret_input=''
terminal_echo_disabled=0

restore_terminal_echo() {
    if [ "$terminal_echo_disabled" -eq 1 ]; then
        stty echo 2>/dev/null || true
        terminal_echo_disabled=0
    fi
}

clear_secrets() {
    restore_terminal_echo
    if [ -n "$secret_writer_pids" ]; then
        # A failed Vault request may stop before opening every FIFO. Terminate
        # any writer still blocked on open so cleanup itself cannot hang.
        kill $secret_writer_pids 2>/dev/null || true
        wait $secret_writer_pids 2>/dev/null || true
    fi
    if [ -n "$secret_pipe_dir" ]; then
        rm -rf "$secret_pipe_dir"
    fi
    secret_writer_pids=''
    secret_pipe_dir=''
    secret_input=''
    vault_token=''
    binance_api_key=''
    binance_api_secret=''
    okx_api_key=''
    okx_api_secret=''
    okx_api_passphrase=''
    binance_account_uid=''
    okx_account_uid=''
    unset VAULT_TOKEN
}
trap clear_secrets EXIT HUP INT TERM
if [ "$mode" = account-proof ]; then
    completion_marker=${COMPLETION_MARKER:-/vault/run/testnet-venue-account-proof.complete}
else
    completion_marker=${COMPLETION_MARKER:-/vault/run/testnet-venue-secrets.complete}
fi
rm -f "$completion_marker"

require_secret() {
    if [ -z "$1" ]; then
        printf '%s\n' 'A required value was empty; no further changes were made.' >&2
        exit 1
    fi
}

read_hidden() {
    printf '%s' "$1" >&2
    if [ -t 0 ]; then
        stty -echo
        terminal_echo_disabled=1
    fi
    if ! IFS= read -r secret_input; then
        restore_terminal_echo
        printf '\n%s\n' 'Input ended before a required value was read; no further changes were made.' >&2
        exit 1
    fi
    restore_terminal_echo
    printf '\n' >&2
    require_secret "$secret_input"
}

write_venue_credentials() {
    venue_path=$1
    shift

    pipe_root=${VENUE_SECRET_FIFO_ROOT:-/vault/run}
    secret_pipe_dir=$(mktemp -d "$pipe_root/venue-secrets.XXXXXX")
    secret_writer_pids=''
    vault_args=''
    field_number=0

    while [ "$#" -gt 0 ]; do
        field_name=$1
        field_value=$2
        shift 2
        field_number=$((field_number + 1))
        field_pipe=$secret_pipe_dir/field-$field_number
        mkfifo "$field_pipe"
        chmod 0600 "$field_pipe"
        printf '%s' "$field_value" > "$field_pipe" &
        secret_writer_pids="$secret_writer_pids $!"
        # Only the FIFO path enters argv. The credential remains in memory and
        # crosses into Vault through the pipe opened by Vault's @file syntax.
        vault_args="$vault_args $field_name=@$field_pipe"
    done

    if "$VAULT_BIN" kv metadata get "$venue_path" >/dev/null 2>&1; then
        vault_action=patch
    else
        vault_action=put
    fi

    set +e
    # Field names and generated FIFO paths contain no shell metacharacters.
    # Splitting vault_args is intentional: Vault requires one key=@file argv
    # entry per field.
    # shellcheck disable=SC2086
    "$VAULT_BIN" kv "$vault_action" "$venue_path" $vault_args >/dev/null
    vault_status=$?
    set -e

    if [ "$vault_status" -ne 0 ]; then
        kill $secret_writer_pids 2>/dev/null || true
    fi
    wait $secret_writer_pids 2>/dev/null || true
    secret_writer_pids=''
    rm -rf "$secret_pipe_dir"
    secret_pipe_dir=''
    return "$vault_status"
}

read_hidden 'Vault operator token: '
vault_token=$secret_input
secret_input=''
VAULT_TOKEN=$vault_token
export VAULT_TOKEN
vault_token=''

"$VAULT_BIN" token lookup >/dev/null

if [ "$mode" = bootstrap ]; then
    if ! "$VAULT_BIN" audit list -format=json | grep -q '"file/"'; then
        "$VAULT_BIN" audit enable -path=file file file_path=/vault/data/audit.log
    fi

    if ! "$VAULT_BIN" secrets list -format=json | grep -q '"kv/"'; then
        "$VAULT_BIN" secrets enable -path=kv -version=2 kv
    fi

    if ! "$VAULT_BIN" auth list -format=json | grep -q '"kubernetes/"'; then
        "$VAULT_BIN" auth enable -path=kubernetes kubernetes
    fi

    "$VAULT_BIN" write auth/kubernetes/config \
        kubernetes_host=https://kubernetes.default.svc:443

    "$VAULT_BIN" policy write venue-binance-read - <<'EOF'
path "kv/data/kanz/venue-binance" {
  capabilities = ["read"]
}
path "kv/metadata/kanz/venue-binance" {
  capabilities = ["read"]
}
path "kv/data/kanz/account-master/binance" {
  capabilities = ["read"]
}
path "kv/metadata/kanz/account-master/binance" {
  capabilities = ["read"]
}
EOF

    "$VAULT_BIN" policy write venue-okx-read - <<'EOF'
path "kv/data/kanz/venue-okx" {
  capabilities = ["read"]
}
path "kv/metadata/kanz/venue-okx" {
  capabilities = ["read"]
}
path "kv/data/kanz/account-master/okx" {
  capabilities = ["read"]
}
path "kv/metadata/kanz/account-master/okx" {
  capabilities = ["read"]
}
EOF

    "$VAULT_BIN" write auth/kubernetes/role/venue-binance \
        bound_service_account_names=venue-binance \
        bound_service_account_namespaces=kanz-services \
        audience=vault \
        token_policies=venue-binance-read \
        token_no_default_policy=true \
        token_type=service \
        token_ttl=5m \
        token_max_ttl=15m

    "$VAULT_BIN" write auth/kubernetes/role/venue-okx \
        bound_service_account_names=venue-okx \
        bound_service_account_namespaces=kanz-services \
        audience=vault \
        token_policies=venue-okx-read \
        token_no_default_policy=true \
        token_type=service \
        token_ttl=5m \
        token_max_ttl=15m
fi

if [ "$mode" = account-proof ]; then
    # Policy writes replace the whole policy. Repeat both the credential path and
    # the independent account-master path so this mode is safe on an estate
    # bootstrapped by an older revision of this script.
    "$VAULT_BIN" policy write venue-binance-read - <<'EOF'
path "kv/data/kanz/venue-binance" {
  capabilities = ["read"]
}
path "kv/metadata/kanz/venue-binance" {
  capabilities = ["read"]
}
path "kv/data/kanz/account-master/binance" {
  capabilities = ["read"]
}
path "kv/metadata/kanz/account-master/binance" {
  capabilities = ["read"]
}
EOF
    "$VAULT_BIN" policy write venue-okx-read - <<'EOF'
path "kv/data/kanz/venue-okx" {
  capabilities = ["read"]
}
path "kv/metadata/kanz/venue-okx" {
  capabilities = ["read"]
}
path "kv/data/kanz/account-master/okx" {
  capabilities = ["read"]
}
path "kv/metadata/kanz/account-master/okx" {
  capabilities = ["read"]
}
EOF

    printf '%s\n' 'Enter UIDs only from independently reviewed exchange account-opening evidence.' >&2
    printf '%s\n' 'Do not copy the adapter observation or derive either value from the mounted API credential.' >&2
    read_hidden 'Approved Binance testnet account UID: '
    binance_account_uid=$secret_input
    secret_input=''
    printf '%s' "$binance_account_uid" | "$VAULT_BIN" kv put kv/kanz/account-master/binance expected_uid=- >/dev/null
    binance_account_uid=''

    read_hidden 'Approved OKX demo account UID: '
    okx_account_uid=$secret_input
    secret_input=''
    printf '%s' "$okx_account_uid" | "$VAULT_BIN" kv put kv/kanz/account-master/okx expected_uid=- >/dev/null
    okx_account_uid=''

    "$VAULT_BIN" kv metadata get kv/kanz/account-master/binance >/dev/null
    "$VAULT_BIN" kv metadata get kv/kanz/account-master/okx >/dev/null
    : > "$completion_marker"
    printf '%s\n' 'ACCOUNT_PROOF_COMPLETE'
    printf '%s\n' 'Both expected account UIDs are stored under separate account-master paths.'
    exit 0
fi

read_hidden 'Binance testnet API key: '
binance_api_key=$secret_input
secret_input=''

read_hidden 'Binance testnet API secret: '
binance_api_secret=$secret_input
secret_input=''

read_hidden 'OKX demo API key: '
okx_api_key=$secret_input
secret_input=''

read_hidden 'OKX demo API secret: '
okx_api_secret=$secret_input
secret_input=''

read_hidden 'OKX demo API passphrase: '
okx_api_passphrase=$secret_input
secret_input=''

# All required values are present before the first mutation. Each call creates
# one complete KV version for that venue and preserves unrelated fields (the
# database DSNs share these paths) through `kv patch` on existing records.
write_venue_credentials kv/kanz/venue-binance \
    api_key "$binance_api_key" \
    api_secret "$binance_api_secret"
unset binance_api_key binance_api_secret

write_venue_credentials kv/kanz/venue-okx \
    api_key "$okx_api_key" \
    api_secret "$okx_api_secret" \
    api_passphrase "$okx_api_passphrase"
unset okx_api_key okx_api_secret okx_api_passphrase

"$VAULT_BIN" kv metadata get kv/kanz/venue-binance >/dev/null
"$VAULT_BIN" kv metadata get kv/kanz/venue-okx >/dev/null
: > "$completion_marker"

printf '%s\n' 'BOOTSTRAP_COMPLETE'
if [ "$mode" = bootstrap ]; then
    printf '%s\n' 'Vault Kubernetes auth, least-privilege venue roles, audit logging, and testnet credentials are configured.'
else
    printf '%s\n' 'The Binance testnet and OKX demo credential versions were rotated.'
fi
printf '%s\n' 'Keep the initial root token offline until durable operator authentication is commissioned; then revoke it.'
