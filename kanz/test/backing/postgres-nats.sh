#!/usr/bin/env bash
# Start the two local test services with plaintext development clients.
set -euo pipefail
export MSYS2_ARG_CONV_EXCL='*'
pg_port=${PG_PORT:-5432}
nats_port=${NATS_PORT:-4222}
suffix=${1:-}
[[ -z "$suffix" || "$suffix" =~ ^-[a-z0-9-]+$ ]] || { echo 'Invalid fixture suffix' >&2; exit 1; }
for port in "$pg_port" "$nats_port"; do
  [[ "$port" =~ ^[1-9][0-9]{0,4}$ && "$port" -le 65535 ]] || { echo 'Invalid published port' >&2; exit 1; }
done
pg_endpoint="127.0.0.1:$pg_port"
nats_endpoint="127.0.0.1:$nats_port"
ids=()
cleanup() { if ((${#ids[@]})); then docker rm -fv "${ids[@]}" >/dev/null; fi; }
trap cleanup EXIT
pg=$(docker create --name "kanz-ci-postgres$suffix" -p "$pg_endpoint:5432" \
  -e POSTGRES_USER=kanz -e POSTGRES_PASSWORD=kanz -e POSTGRES_DB=kanz \
  postgres:16-alpine@sha256:1fb72075a1de85a1d39de23c2da81baef1b71bf5c0b61a14cbbfdb072a6246d9)
ids+=("$pg")
config="$(cd "$(dirname "$0")" && pwd)/nats-dev.conf"
if command -v cygpath >/dev/null; then config=$(cygpath -w "$config"); fi
nats=$(docker create --name "kanz-ci-nats$suffix" -p "$nats_endpoint:4222" \
  -v "$config:/etc/nats/nats-dev.conf:ro" \
  nats@sha256:d4ac35882ac65aff236cd65b9d3fa4d24332c681e1a85f94eedccd3cdd65b1da -c /etc/nats/nats-dev.conf)
ids+=("$nats")
docker start "$pg" "$nats" >/dev/null
[[ "$(docker port "$pg" 5432/tcp)" == "$pg_endpoint" && "$(docker port "$nats" 4222/tcp)" == "$nats_endpoint" ]]
for _ in $(seq 1 60); do
  if docker exec "$pg" pg_isready -h 127.0.0.1 -U kanz >/dev/null && \
      [[ "$(docker logs "$nats" 2>&1)" == *"Server is ready"* ]]; then break; fi
  sleep 1
done
docker exec "$pg" pg_isready -h 127.0.0.1 -U kanz >/dev/null
[[ "$(docker logs "$nats" 2>&1)" == *"Server is ready"* ]]
# --- The application role ----------------------------------------------------
# Created NOSUPERUSER, then VERIFIED. Creating it correctly and checking it are
# different acts: a future edit here, a hand-patched local database, or a role
# that already existed with different attributes all produce a DSN that looks
# right and silently disables RLS.
docker exec "$pg" psql -U kanz -d kanz -q -v ON_ERROR_STOP=1 \
  -c "CREATE ROLE kanzapp LOGIN PASSWORD 'kanz' NOSUPERUSER;" \
  -c "CREATE DATABASE kanzapp OWNER kanzapp;" >/dev/null
docker exec "$pg" psql -U kanz -d kanzapp -q -v ON_ERROR_STOP=1 \
  -c "GRANT ALL ON SCHEMA public TO kanzapp;" >/dev/null

# A single deterministic token rather than two booleans: psql renders bool as
# `t`/`f` unquoted but `false` under ::text, so comparing the raw values invites
# a check that is really testing psql's formatting. An absent role yields an
# empty string, which is also not CONSTRAINED — so a missing role fails here too
# rather than sailing past a comparison that only ever saw the happy shape.
attrs="$(docker exec "$pg" psql -U kanz -d kanz -tAc \
  "select case when rolsuper or rolbypassrls then 'BYPASSES_RLS' else 'CONSTRAINED' end
     from pg_roles where rolname = 'kanzapp'")"
attrs="$(echo "$attrs" | tr -d '\r\n')"
if [ "$attrs" != "CONSTRAINED" ]; then
  echo "FAIL: the kanzapp role is not RLS-constrained (got '$attrs', want 'CONSTRAINED')."
  echo "      A superuser — or any role with BYPASSRLS — is exempt from every row-level"
  echo "      security policy in the schema. The multi-tenant isolation tests would still"
  echo "      PASS, because the policies they exist to prove would never be consulted."
  echo "      That is a false green, not a configuration detail. Refusing to hand out a DSN."
  exit 1
fi
echo "  role kanzapp verified NOSUPERUSER / NOBYPASSRLS" >&2

printf "export TEST_POSTGRES_URL='postgres://kanzapp:kanz@%s/kanzapp?sslmode=disable'\n" "$pg_endpoint"
printf "export TEST_NATS_URL='nats://%s'\n" "$nats_endpoint"
trap - EXIT
