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

# --- The NATS topology -------------------------------------------------------
# Extracted from the ConfigMap production applies, never copied — same stance as
# test/mtls/up.sh, and for the same reason: a copy drifts and the drift is
# invisible until a service meets a real spine. A JetStream broker with no
# streams answers "no response from stream", which reads as a transport fault
# and is not one.
awk '/^  bootstrap\.sh: \|/{f=1;next} f&&/^---/{exit} f{sub(/^    /,"");print}' \
  "$ROOT/infra/nats/bootstrap-job.yaml" > /tmp/kanz-backing-bootstrap.sh
test -s /tmp/kanz-backing-bootstrap.sh || { echo "FAIL: could not extract bootstrap.sh from infra/nats/bootstrap-job.yaml"; exit 1; }

NATS_BOX_IMAGE="$(awk '/image: *natsio\/nats-box/{sub(/^[[:space:]]*image:[[:space:]]*/,"");print;exit}' \
  "$ROOT/infra/nats/bootstrap-job.yaml")"
test -n "$NATS_BOX_IMAGE" || { echo "FAIL: could not read the nats-box image from infra/nats/bootstrap-job.yaml"; exit 1; }

docker run --rm -i --network host \
  -e NATS_URL="nats://127.0.0.1:${NATS_PORT}" -e NATS_REPLICAS=1 \
  "$NATS_BOX_IMAGE" sh -s < /tmp/kanz-backing-bootstrap.sh > /tmp/kanz-backing-bootstrap.log 2>&1 || {
    echo "FAIL: the bootstrap script did not complete"; cat /tmp/kanz-backing-bootstrap.log; exit 1; }

# Derived from the script, never listed here: a stream added to the Job is
# required by this check the moment it is added.
EXPECTED="$(awk '/^[[:space:]]*ensure_stream[[:space:]]+[A-Z]/{print $2}' /tmp/kanz-backing-bootstrap.sh | sort -u)"
test -n "$EXPECTED" || { echo "FAIL: parsed no stream names out of bootstrap.sh — the check would be vacuous"; exit 1; }
MISSING=""
for s in $EXPECTED; do grep -qw "$s" /tmp/kanz-backing-bootstrap.log || MISSING="$MISSING $s"; done
[ -z "$MISSING" ] || { echo "FAIL: bootstrap ran but these streams are absent:$MISSING"; cat /tmp/kanz-backing-bootstrap.log; exit 1; }
echo "  NATS topology provisioned: $(echo $EXPECTED | wc -w) streams"

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
