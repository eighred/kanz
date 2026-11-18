#!/usr/bin/env bash
# Stand up the four backing services the test suite gates on. Local Kafka API
# tests use Redpanda; CI also exercises the legacy Kafka implementation.
#
# WHY THIS EXISTS. Without them, `go test ./...` is GREEN AND HOLLOW. Measured on
# a clean checkout: across the 24 packages that read TEST_POSTGRES_URL,
# TEST_NATS_URL or TEST_KAFKA_BROKERS, **73 tests skip** — and they skip
# SILENTLY, one t.Skip line each, inside an otherwise passing run. Nothing in the
# output says a third of the integration surface did not execute. AGENTS.md warns
# about exactly this for Postgres ("TEST_POSTGRES_URL unset ⇒ 14 test files skip
# silently"); the same trap covers the bus and Kafka, and the only reason it
# stayed invisible is that the recipe lived in a CI workflow nobody runs locally.
#
# With these up the same command runs those 73. That is the whole point: a laptop
# and CI should disagree about speed, not about coverage.
#
# THE ROLE MUST BE NOSUPERUSER, AND THIS SCRIPT REFUSES TO PROCEED OTHERWISE.
# A superuser BYPASSES ROW-LEVEL SECURITY. Point the suite at a superuser DSN and
# the multi-tenant isolation tests still pass — they pass because RLS was never
# consulted, which is indistinguishable from passing because RLS works. That is
# the single most dangerous false green available here, so the check is an
# assertion against pg_roles rather than a comment, and it fails loudly.
#
# Usage:  test/backing/up.sh          then eval the exports it prints
#   Optional argument: a unique suffix such as -smoke-42. Use the printed IDs for teardown.
#
# The mTLS broker is SEPARATE (test/mtls/up.sh) and deliberately so: this one is
# the plaintext broker the semantics suite uses, that one proves the production
# transport. Neither substitutes for the other.
set -euo pipefail

PG_PORT="${PG_PORT:-5432}"
NATS_PORT="${NATS_PORT:-4222}"
KAFKA_PORT="${KAFKA_PORT:-9092}"
REDIS_PORT="${REDIS_PORT:-6379}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export MSYS2_ARG_CONV_EXCL="*"
export MSYS_NO_PATHCONV=1

suffix=${1:-}
[[ $# -le 1 && ( -z "$suffix" || "$suffix" =~ ^-[a-z0-9-]+$ ) ]] || { echo 'Invalid fixture suffix' >&2; exit 1; }
KANZ_BACKING_RUN=$(python3 -c 'import uuid; print(uuid.uuid4().hex)')
export KANZ_BACKING_RUN
owned_ids() { docker ps -aq --no-trunc --filter "label=io.kanz.backing.run=$KANZ_BACKING_RUN"; }
rollback() {
  local rc=$? ids
  ids=$(owned_ids) || exit 1
  if [[ -n "$ids" ]]; then
    local -a owned
    mapfile -t owned <<<"$ids"
    docker rm -fv "${owned[@]}" >/dev/null || exit 1
    ids=$(owned_ids) && [[ -z "$ids" ]] || exit 1
  fi
  exit "$rc"
}
trap rollback EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

backing_exports="$(PG_PORT="$PG_PORT" NATS_PORT="$NATS_PORT" bash "$ROOT/test/backing/postgres-nats.sh" "$suffix")"

# --- Valkey (the `-tags redis` suite) ---------------------------------------
valkey_exports="$(REDIS_PORT="$REDIS_PORT" bash "$ROOT/test/backing/valkey.sh" "kanz-ci-redis$suffix")"

# --- Redpanda, single-node Kafka API -----------------------------------------
kafka_exports="$(KAFKA_PORT="$KAFKA_PORT" bash "$ROOT/test/backing/kafka.sh" "kanz-ci-kafka$suffix")"

# Bootstrap from the canonical ConfigMap, then check the broker's current
# stream inventory before printing any connection settings. Requires PyYAML.
python3 -B "$ROOT/test/backing/nats_topology.py" bootstrap "nats://127.0.0.1:${NATS_PORT}"

ids=$(owned_ids)
mapfile -t owned <<<"$ids"
[[ ${#owned[@]} == 4 ]] || { echo 'Backing stack ownership incomplete' >&2; exit 1; }
cat <<EOF

All four backing services are up. Export these, then run the suite:

  $backing_exports
  $kafka_exports
  $valkey_exports
  go test -p 1 ./...
  go test -tags redis -p 1 ./pkg/redisadapter ./services/webhook_ingest/... ./services/api_gateway/...

  Tear down this stack: docker rm -fv ${owned[*]}

ORDER WARNING — run any real migration BEFORE the suite, never after.
internal/migrate's tests share this database and DROP schema_migrations at
setup, leaving their own fixture rows behind. Run \`kanz-migrate\` afterwards and
it collides at version 1; run it before and the suite erases its ledger. CI is
only safe here because its OMS migrate step precedes \`go test\`. Tracked
separately — do not work around it by editing a migration.
EOF
trap - EXIT INT TERM
