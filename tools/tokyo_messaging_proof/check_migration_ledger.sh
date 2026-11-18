#!/usr/bin/env bash
set -euo pipefail
export MSYS2_ARG_CONV_EXCL='*'
source "$(dirname "$0")/../testnet-data-plane-recovery.sh"
image='postgres:16-alpine@sha256:1fb72075a1de85a1d39de23c2da81baef1b71bf5c0b61a14cbbfdb072a6246d9'
id=$(docker create --network none --tmpfs /run/secrets/postgres:mode=0700 -e POSTGRES_HOST_AUTH_METHOD=trust "$image")
trap 'docker rm -fv "$id" >/dev/null' EXIT
docker start "$id" >/dev/null
# TCP readiness avoids the entrypoint's short-lived Unix-only init server.
for _ in $(seq 1 60); do docker exec "$id" pg_isready -h 127.0.0.1 -U postgres >/dev/null && break; sleep 1; done
docker exec "$id" psql -Xq -v ON_ERROR_STOP=1 -U postgres -c 'CREATE ROLE kanz_restore LOGIN NOSUPERUSER NOBYPASSRLS'
for db in source restored; do docker exec "$id" createdb -U postgres -O kanz_restore "$db"; done
psql_db() { docker exec "$id" psql -XAtq -v ON_ERROR_STOP=1 -U kanz_restore -d "$1" -c "$2"; }
docker exec "$id" sh -ceu 'printf 127.0.0.1 > /run/secrets/postgres/host; : > /run/secrets/postgres/password'
k() {
  case "${*:1:7}" in
    '-n kanz-data exec fixture-primary -c postgres --'|'-n kanz-recovery-drill exec postgres-restore -c restore --') ;;
    *) return 1 ;;
  esac
  shift 7; docker exec "$id" "$@"
}
query=$(migration_ledger_sql)
psql_db source "CREATE TABLE schema_migrations (version bigint PRIMARY KEY, name text NOT NULL, checksum text NOT NULL);
  INSERT INTO schema_migrations VALUES (2,'two','bb'),(1,'one','aa')"
legacy=$(psql_db source "$query")
psql_db source "ALTER TABLE schema_migrations ADD COLUMN set_name text NOT NULL DEFAULT ''"
verify_migration_ledger "$legacy" "$(psql_db source "$query")"
psql_db source "UPDATE schema_migrations SET set_name='events' WHERE version=2"
row=$(source_postgres_inventory_row fixture-primary source)
[[ "$row" =~ ^source\|2\|1\|0\|[0-9A-F]+/[0-9A-F]+\|([0-9a-f]{64})$ ]]
expected=${BASH_REMATCH[1]}
docker exec "$id" pg_dump -U kanz_restore -d source -Fc --no-owner --no-privileges |
  docker exec -i "$id" pg_restore -U kanz_restore -d restored --no-owner --no-privileges --exit-on-error
verify_migration_ledger "$expected" "$(restored_migration_ledger restored)"
for edit in "name='other'" "checksum='cc'" "set_name='other'" "version=3"; do
  observed=$(psql_db restored "BEGIN; UPDATE schema_migrations SET $edit WHERE version=2; $query; ROLLBACK;")
  [[ "$observed" =~ ^[0-9a-f]{64}$ ]]
  if (verify_migration_ledger "$expected" "$observed"); then echo 'Equal-count substitution accepted' >&2; exit 1; fi
done
[[ "$(psql_db restored 'select count(*) from schema_migrations')" == 2 ]]
verify_migration_ledger "$expected" "$(restored_migration_ledger restored)"
for format in v1 v2; do
  evidence=$(migration_ledger_evidence "kanz-testnet-data-plane-$format")
  state=$(jq -nr --argjson ledger "$evidence" '$ledger.state')
  if [[ "$format" == v1 ]]; then [[ "$state" == UNKNOWN ]]; else [[ "$state" == VERIFIED ]]; fi
done
echo 'Verified migration ledger after dump/restore, four equal-count refusals, and legacy unnamed set'
