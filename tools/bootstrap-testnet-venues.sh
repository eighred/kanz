#!/bin/sh

# Repository-owned Vault bootstrap and credential rotation for testnet adapters.
# Run interactively inside the vault container. Secret values are accepted only
# on stdin and are never written to this file, command arguments, or stdout.

set -eu
umask 077

mode=${1:-bootstrap}
case "$mode" in
    bootstrap|rotate) ;;
    *)
        printf 'usage: %s [bootstrap|rotate]\n' "$0" >&2
        exit 2
        ;;
esac

VAULT_BIN=${VAULT_BIN:-/bin/vault}
export VAULT_ADDR=https://127.0.0.1:8200
export VAULT_CACERT=/run/spire/certs/bundle.crt
export VAULT_TLS_SERVER_NAME=vault.vault.svc.cluster.local

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

clear_secrets() {
    vault_token=''
    binance_api_key=''
    binance_api_secret=''
    okx_api_key=''
    okx_api_secret=''
    okx_api_passphrase=''
    unset VAULT_TOKEN
}
trap clear_secrets EXIT HUP INT TERM
rm -f /vault/run/testnet-venue-secrets.complete

require_secret() {
    if [ -z "$1" ]; then
        printf '%s\n' 'A required value was empty; no further changes were made.' >&2
        exit 1
    fi
}

printf '%s' 'Vault operator token: ' >&2
IFS= read -r -s vault_token
printf '\n' >&2
require_secret "$vault_token"
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
EOF

    "$VAULT_BIN" policy write venue-okx-read - <<'EOF'
path "kv/data/kanz/venue-okx" {
  capabilities = ["read"]
}
path "kv/metadata/kanz/venue-okx" {
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

printf '%s' 'Binance testnet API key: ' >&2
IFS= read -r -s binance_api_key
printf '\n' >&2
require_secret "$binance_api_key"
if "$VAULT_BIN" kv metadata get kv/kanz/venue-binance >/dev/null 2>&1; then
    printf '%s' "$binance_api_key" | "$VAULT_BIN" kv patch kv/kanz/venue-binance api_key=- >/dev/null
else
    printf '%s' "$binance_api_key" | "$VAULT_BIN" kv put kv/kanz/venue-binance api_key=- >/dev/null
fi
binance_api_key=''

printf '%s' 'Binance testnet API secret: ' >&2
IFS= read -r -s binance_api_secret
printf '\n' >&2
require_secret "$binance_api_secret"
printf '%s' "$binance_api_secret" | "$VAULT_BIN" kv patch kv/kanz/venue-binance api_secret=- >/dev/null
binance_api_secret=''

printf '%s' 'OKX demo API key: ' >&2
IFS= read -r -s okx_api_key
printf '\n' >&2
require_secret "$okx_api_key"
if "$VAULT_BIN" kv metadata get kv/kanz/venue-okx >/dev/null 2>&1; then
    printf '%s' "$okx_api_key" | "$VAULT_BIN" kv patch kv/kanz/venue-okx api_key=- >/dev/null
else
    printf '%s' "$okx_api_key" | "$VAULT_BIN" kv put kv/kanz/venue-okx api_key=- >/dev/null
fi
okx_api_key=''

printf '%s' 'OKX demo API secret: ' >&2
IFS= read -r -s okx_api_secret
printf '\n' >&2
require_secret "$okx_api_secret"
printf '%s' "$okx_api_secret" | "$VAULT_BIN" kv patch kv/kanz/venue-okx api_secret=- >/dev/null
okx_api_secret=''

printf '%s' 'OKX demo API passphrase: ' >&2
IFS= read -r -s okx_api_passphrase
printf '\n' >&2
require_secret "$okx_api_passphrase"
printf '%s' "$okx_api_passphrase" | "$VAULT_BIN" kv patch kv/kanz/venue-okx api_passphrase=- >/dev/null
okx_api_passphrase=''

"$VAULT_BIN" kv metadata get kv/kanz/venue-binance >/dev/null
"$VAULT_BIN" kv metadata get kv/kanz/venue-okx >/dev/null
: > /vault/run/testnet-venue-secrets.complete

printf '%s\n' 'BOOTSTRAP_COMPLETE'
if [ "$mode" = bootstrap ]; then
    printf '%s\n' 'Vault Kubernetes auth, least-privilege venue roles, audit logging, and testnet credentials are configured.'
else
    printf '%s\n' 'The Binance testnet and OKX demo credential versions were rotated.'
fi
printf '%s\n' 'Keep the initial root token offline until durable operator authentication is commissioned; then revoke it.'
