#!/usr/bin/env bash
# One command to get the web surface running locally (#371).
#
# It installs kanz-web's dependencies, builds the SPA, and prints the exact
# environment the BFF needs to serve it. It does NOT start anything: the local
# rig is `kanz/test/backing/up.sh`, and a second script that starts services
# would be a second answer to "how do I run this".
#
# PORTS ARE CHOSEN NOT TO COLLIDE with the rig (kanz/test/backing/up.sh):
#   Postgres 5432 · NATS 4222 · NATS-mTLS 4223 · Kafka 9092 · Redis 6379
# leaving the BFF on 8084 (WEB_BFF_LISTEN's own default) and Vite on 5173.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WEB="$ROOT/kanz-web"
BFF_INTERNAL_PORT="${BFF_INTERNAL_PORT:-8084}"

command -v npm >/dev/null 2>&1 || { echo "FAIL: npm is required (Node 20+)"; exit 1; }

echo "==> installing kanz-web dependencies"
( cd "$WEB" && npm ci --no-audit --no-fund 2>/dev/null || npm install --no-audit --no-fund )

echo "==> building the SPA"
( cd "$WEB" && npm run build )

test -f "$WEB/dist/index.html" || { echo "FAIL: the build produced no dist/index.html"; exit 1; }
echo "    built: $WEB/dist"

cat <<BANNER

The SPA is built. Two processes serve it, in this order.

1. THE IDENTITY SERVICE — where a credential becomes a token. Its schema is
   applied by cmd/kanz-migrate, not by the service:

  export IDENTITY_DATABASE_URL='postgres://kanzapp:kanz@localhost:5432/kanzapp?sslmode=disable'
  export IDENTITY_TOKEN_ISSUER='http://127.0.0.1:8087'
  export IDENTITY_TOKEN_AUDIENCE='kanz-api'
  export IDENTITY_ALLOW_EPHEMERAL_KEY=true   # LOCAL ONLY — see the warning below
  go run ./services/identity/cmd/identity

2. THE BFF — ONE ORIGIN, so the httpOnly session cookie needs no CORS and no
   cookie-domain syncing:

  export WEB_BFF_STATIC_DIR='$WEB/dist'
  export WEB_BFF_LISTEN=':$BFF_INTERNAL_PORT'
  export WEB_BFF_IDENTITY_URL='http://127.0.0.1:8087'
  export WEB_BFF_GATEWAY_URL='http://127.0.0.1:8080'    # the api-gateway
  export WEB_BFF_INSECURE_COOKIES=1                     # LOCAL HTTP ONLY
  go run ./services/web-bff/cmd/web-bff

  open http://127.0.0.1:$BFF_INTERNAL_PORT

3. AN ACCOUNT TO SIGN IN WITH. There is no self-registration: accounts are
   provisioned, and the FIRST one has to be created by whoever holds the
   database credential, because there is no operator yet to authorise it.

  go run ./cmd/kanz-invite -subject user:you -tenant acme       -roles kanz-operator,kanz-trader -by bootstrap

   It prints a single-use token ONCE — only its SHA-256 is stored. Redeem it on
   the sign-in page; redeeming sets the password AND signs you in.

IDENTITY_ALLOW_EPHEMERAL_KEY generates a NEW signing key every start, so every
restart invalidates every session issued before it. That is fine on a laptop and
never anywhere else; set IDENTITY_SIGNING_KEY_FILE to a PEM instead:

  openssl ecparam -genkey -name prime256v1 -noout -out identity-key.pem

For UI work with hot reload instead, run Vite and let it proxy /api and /auth
back to the BFF — the dev shape stays same-origin, so the cookie behaves the
same as production:

  ( cd '$WEB' && BFF_INTERNAL_PORT=$BFF_INTERNAL_PORT npm run dev )

WEB_BFF_INSECURE_COOKIES IS LOCAL-ONLY. It drops the Secure flag so a cookie
works over plain http; setting it in a deployment sends the session id over any
downgraded connection. The edge terminates TLS, so a deployment never needs it.
BANNER
