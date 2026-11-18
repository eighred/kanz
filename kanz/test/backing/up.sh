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
#   Tear down: docker rm -f kanz-ci-postgres kanz-ci-redis kanz-ci-nats kanz-ci-kafka
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

docker rm -f kanz-ci-postgres kanz-ci-redis kanz-ci-nats kanz-ci-kafka >/dev/null 2>&1 || true

backing_exports="$(PG_PORT="$PG_PORT" NATS_PORT="$NATS_PORT" bash "$ROOT/test/backing/postgres-nats.sh")"

# --- Valkey (the `-tags redis` suite) ---------------------------------------
valkey_exports="$(REDIS_PORT="$REDIS_PORT" bash "$ROOT/test/backing/valkey.sh")"

# --- Redpanda, single-node Kafka API -----------------------------------------
kafka_exports="$(KAFKA_PORT="$KAFKA_PORT" bash "$ROOT/test/backing/kafka.sh")"

# Bootstrap from the canonical ConfigMap, then check the broker's current
# stream inventory before printing any connection settings. Requires PyYAML.
python3 -B "$ROOT/test/backing/nats_topology.py" bootstrap "nats://127.0.0.1:${NATS_PORT}"

cat <<EOF

All four backing services are up. Export these, then run the suite:

  $backing_exports
  $kafka_exports
  $valkey_exports
  go test -p 1 ./...
  go test -tags redis -p 1 ./pkg/redisadapter ./services/webhook_ingest/... ./services/api_gateway/...

ORDER WARNING — run any real migration BEFORE the suite, never after.
internal/migrate's tests share this database and DROP schema_migrations at
setup, leaving their own fixture rows behind. Run \`kanz-migrate\` afterwards and
it collides at version 1; run it before and the suite erases its ledger. CI is
only safe here because its OMS migrate step precedes \`go test\`. Tracked
separately — do not work around it by editing a migration.
EOF
