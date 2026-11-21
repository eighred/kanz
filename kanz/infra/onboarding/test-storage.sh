#!/bin/sh
# Run the storage step against both migrated databases in a disposable local fixture.
set -eu
cd "$(dirname "$0")"
case "$(uname -s)" in
  MINGW*|MSYS*) endpoint=npipe:////./pipe/docker_engine; export MSYS_NO_PATHCONV=1 ;;
  *) endpoint=unix:///var/run/docker.sock ;;
esac
d() { docker --host "$endpoint" "$@"; }
image=postgres@sha256:aa6cb33cf4c19c765cefcf48d96176258800870a9a1e930ad126200f9d825139
fixture=$(d run --detach --rm --pull=never --network=none --tmpfs /var/lib/postgresql/data \
  --tmpfs /run/proof -e POSTGRES_PASSWORD_FILE=/run/proof/bootstrap \
  -e 'POSTGRES_INITDB_ARGS=--auth-local=peer --auth-host=scram-sha-256' --entrypoint sh "$image" -ec '
    umask 077
    head -c 32 /dev/urandom | base64 > /run/proof/bootstrap
    chown postgres /run/proof/bootstrap
    exec docker-entrypoint.sh postgres -c listen_addresses=""')
trap 'd rm -f "$fixture" >/dev/null' EXIT
for attempt in $(seq 1 60); do
  if d exec -u postgres "$fixture" sh -c \
    'test "$(head -1 "$PGDATA/postmaster.pid" 2>/dev/null)" = 1 && pg_isready -q'; then break; fi
  [ "$attempt" -lt 60 ] || { echo "PostgreSQL startup timed out" >&2; exit 1; }
  sleep 1
done
sql() { role=$1; shift; d exec -i -u "$role" "$fixture" psql -XqAt -v ON_ERROR_STOP=1 "$@"; }
for role in migration_owner onboard_check; do
  d exec "$fixture" adduser -D "$role"
  sql postgres -d postgres -c "CREATE ROLE $role LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE"
done
sql postgres -d postgres -c 'SHOW server_version'
sql postgres -d postgres -c 'ALTER ROLE onboard_check SET search_path TO scratch, public'
for svc in risk_engine accounting; do
  case "$svc" in risk_engine) db=risk ;; accounting) db=books ;; esac
  sql postgres -d postgres -c "CREATE DATABASE $db OWNER migration_owner"
  if [ "$db" = risk ]; then
    sql migration_owner -d risk -c 'CREATE SCHEMA risk_store; ALTER ROLE migration_owner IN DATABASE risk SET search_path TO risk_store, public'
  fi
  for migration in "../../services/$svc/migrations/"*.sql; do
    sql migration_owner -d "$db" -1 -f - < "$migration" >/dev/null
  done
done
# Only the kubectl transport is replaced. The real script runs psql using peer auth.
d exec -i "$fixture" tee /tmp/provision-tenant.sh < provision-tenant.sh >/dev/null
d exec -i "$fixture" sh -ec 'cat > /usr/local/bin/kubectl && chmod 755 /usr/local/bin/kubectl' <<'KUBECTL'
#!/bin/sh
set -eu
[ "$1 $2 $3 $5 $6 $7" = "-n kanz-data exec -- psql -d" ] || exit 2
case "$4:$8" in kanz-books-1:books|kanz-risk-1:risk) ;; *) exit 2 ;; esac
shift 5
exec "$@"
KUBECTL
probe() {
  d exec -u onboard_check "$fixture" env STEP=storage TENANT=synthetic RISK_DB_NAME=risk \
    BOOKS_DB_NAME=books sh /tmp/provision-tenant.sh
}
probe
for db in risk books; do
  case "$db" in risk) schema=risk_store; anchor=portfolios ;; books) schema=public; anchor=ledger_entries ;; esac
  tables=$(sql onboard_check -d "$db" -c "SELECT relname FROM pg_catalog.pg_class
    WHERE relnamespace='$schema'::regnamespace AND relrowsecurity AND relforcerowsecurity ORDER BY 1")
  [ -n "$tables" ] || exit 1
  # A same-name scratch relation must never satisfy the check for a service table.
  sql migration_owner -d "$db" -c 'CREATE SCHEMA scratch; GRANT USAGE ON SCHEMA scratch TO onboard_check'
  for table in $tables; do
    if [ "$table" != "$anchor" ]; then
      sql migration_owner -d "$db" -c "CREATE TABLE scratch.$table ();
        ALTER TABLE scratch.$table ENABLE ROW LEVEL SECURITY;
        ALTER TABLE scratch.$table FORCE ROW LEVEL SECURITY"
    fi
    sql migration_owner -d "$db" -c "ALTER TABLE $schema.$table NO FORCE ROW LEVEL SECURITY"
    result=0
    output=$(probe 2>&1) || result=$?
    if [ "$result" -ne 1 ] || ! printf '%s\n' "$output" | grep -Fq "FATAL: FORCE RLS not active on kanz-$db for: $table ("; then
      printf 'Unexpected storage result %s for %s.%s\n%s\n' "$result" "$db" "$table" "$output" >&2
      exit 1
    fi
    sql migration_owner -d "$db" -c "ALTER TABLE $schema.$table FORCE ROW LEVEL SECURITY"
    echo "Verified storage refuses $db.$table without FORCE"
  done
  sql migration_owner -d "$db" -c "CREATE TABLE scratch.$anchor ()"
  result=0
  output=$(probe 2>&1) || result=$?
  if [ "$result" -ne 1 ] || ! printf '%s\n' "$output" | grep -Fq "could not run the RLS check on kanz-$db"; then
    printf 'Expected refusal for ambiguous %s in %s\n%s\n' "$anchor" "$db" "$output" >&2
    exit 1
  fi
  sql migration_owner -d "$db" -c "DROP TABLE scratch.$anchor"
  echo "Verified storage refuses ambiguous $db.$anchor"
done
probe
