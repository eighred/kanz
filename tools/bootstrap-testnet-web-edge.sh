#!/bin/sh

# One-shot Vault bootstrap for the Tokyo web edge. Both sensitive values are
# entered inside the remote Vault container. They are never printed, passed as
# command arguments, or written outside Vault.
set -eu
umask 077

VAULT_BIN=${VAULT_BIN:-/bin/vault}
VAULT_ADDR=${VAULT_ADDR:-https://127.0.0.1:8200}
VAULT_CACERT=${VAULT_CACERT:-/run/spire/certs/bundle.crt}
VAULT_TLS_SERVER_NAME=${VAULT_TLS_SERVER_NAME:-vault.vault.svc.cluster.local}
COMPLETION_MARKER=${COMPLETION_MARKER:-/vault/run/testnet-web-edge.complete}
export VAULT_ADDR VAULT_CACERT VAULT_TLS_SERVER_NAME

if [ ! -r "$VAULT_CACERT" ]; then
    printf 'Vault trust bundle is not readable; refusing bootstrap.\n' >&2
    exit 1
fi

vault_token=''
tunnel_token=''
terminal_state=''
restore_terminal() {
    if [ -n "$terminal_state" ]; then stty "$terminal_state"; terminal_state=''; fi
}
clear_secrets() {
    restore_terminal
    vault_token=''
    tunnel_token=''
    unset VAULT_TOKEN
}
trap clear_secrets EXIT HUP INT TERM
rm -f "$COMPLETION_MARKER"

hidden_read() {
    label=$1
    printf '%s' "$label" >&2
    if [ -t 0 ]; then terminal_state=$(stty -g); stty -echo; fi
    IFS= read -r secret_input
    restore_terminal
    printf '\n' >&2
    printf '%s' "$secret_input"
    secret_input=''
}

vault_token=$(hidden_read 'Vault operator token: ')
if [ -z "$vault_token" ]; then printf 'The Vault token was empty; no changes were made.\n' >&2; exit 1; fi
VAULT_TOKEN=$vault_token
export VAULT_TOKEN
vault_token=''
"$VAULT_BIN" token lookup >/dev/null

if ! "$VAULT_BIN" secrets list -format=json | grep -q '"kv/"'; then
    printf 'Vault KV is not bootstrapped; run the data-plane bootstrap first.\n' >&2
    exit 1
fi
if ! "$VAULT_BIN" auth list -format=json | grep -q '"kubernetes/"'; then
    printf 'Vault Kubernetes auth is not bootstrapped; run the data-plane bootstrap first.\n' >&2
    exit 1
fi

cat <<'EOF' | "$VAULT_BIN" policy write web-bff-read - >/dev/null
path "kv/data/kanz/api-gateway" {
  capabilities = ["read"]
}
path "kv/metadata/kanz/api-gateway" {
  capabilities = ["read"]
}
path "kv/data/kanz/cloudflare" {
  capabilities = ["read"]
}
path "kv/metadata/kanz/cloudflare" {
  capabilities = ["read"]
}
EOF
"$VAULT_BIN" write auth/kubernetes/role/web-bff \
    bound_service_account_names=web-bff \
    bound_service_account_namespaces=kanz-services \
    audience=vault \
    token_policies=web-bff-read \
    token_no_default_policy=true \
    token_type=service \
    token_ttl=5m \
    token_max_ttl=15m >/dev/null

# Metadata proves whether this dedicated path has been initialized without
# retrieving its value. A rerun preserves the existing tunnel identity.
if ! "$VAULT_BIN" kv metadata get kv/kanz/cloudflare >/dev/null 2>&1; then
    tunnel_token=$(hidden_read 'Cloudflare remotely-managed tunnel token: ')
    if [ -z "$tunnel_token" ]; then printf 'The tunnel token was empty; no token was written.\n' >&2; exit 1; fi
    printf '%s' "$tunnel_token" | "$VAULT_BIN" kv put kv/kanz/cloudflare tunnel_token=- >/dev/null
    tunnel_token=''
fi

"$VAULT_BIN" kv metadata get kv/kanz/api-gateway >/dev/null
"$VAULT_BIN" kv metadata get kv/kanz/cloudflare >/dev/null
: > "$COMPLETION_MARKER"
printf '%s\n' 'WEB_EDGE_BOOTSTRAP_COMPLETE'
printf '%s\n' 'The web-bff Vault role and tunnel-token metadata are present. No credential value left the Vault container.'
