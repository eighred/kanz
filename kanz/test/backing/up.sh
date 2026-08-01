#!/usr/bin/env bash
# Stand up the four backing services the test suite gates on, exactly as
# kanz-ci.yml stands them up.
#
# WHY THIS EXISTS. Without them, `go test ./...` is GREEN AND HOLLOW. Measured on
# a clean checkout: across the 24 packages that read TEST_POSTGRES_URL,
# TEST_NATS_URL or TEST_KAFKA_BROKERS, **73 tests skip** — and they skip
# SILENTLY, one t.Skip line each, inside an otherwise passing run. Nothing in the
# output says a third of the integration surface did not execute. CLAUDE.md warns
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

# --- Postgres ---------------------------------------------------------------
# Image and credentials match kanz-ci.yml's service container.
docker run -d --name kanz-ci-postgres -p "${PG_PORT}:5432" \
  -e POSTGRES_USER=kanz -e POSTGRES_PASSWORD=kanz -e POSTGRES_DB=kanz \
  postgres:16-alpine >/dev/null

# --- Redis (the `-tags redis` suite) ----------------------------------------
docker run -d --name kanz-ci-redis -p "${REDIS_PORT}:6379" redis:7-alpine >/dev/null

# --- NATS, plaintext, JetStream ---------------------------------------------
docker run -d --name kanz-ci-nats -p "${NATS_PORT}:4222" nats:2 -js >/dev/null

# --- Kafka, single-node KRaft -----------------------------------------------
docker run -d --name kanz-ci-kafka -p "${KAFKA_PORT}:9092" \
  -e KAFKA_NODE_ID=1 \
  -e KAFKA_PROCESS_ROLES=broker,controller \
  -e KAFKA_LISTENERS=PLAINTEXT://:9092,CONTROLLER://:9093 \
  -e KAFKA_ADVERTISED_LISTENERS=PLAINTEXT://localhost:9092 \
  -e KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER \
  -e KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT \
  -e KAFKA_CONTROLLER_QUORUM_VOTERS=1@localhost:9093 \
  -e KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1 \
  -e KAFKA_INTER_BROKER_LISTENER_NAME=PLAINTEXT \
  -e KAFKA_AUTO_CREATE_TOPICS_ENABLE=false \
  apache/kafka:3.9.0 >/dev/null

wait_for() { # wait_for <label> <seconds> <command...>
  label="$1"; deadline="$2"; shift 2
  for _ in $(seq 1 "$deadline"); do
    if "$@" >/dev/null 2>&1; then echo "  $label ready"; return 0; fi
    sleep 1
  done
  echo "FAIL: $label did not become ready in ${deadline}s"
  docker logs "kanz-ci-${label}" 2>&1 | tail -20
  exit 1
}
nats_ready()  { docker logs kanz-ci-nats  2>&1 | grep -q "Server is ready"; }
kafka_ready() { docker logs kanz-ci-kafka 2>&1 | grep -q "Kafka Server started"; }

echo "waiting for backing services..."
wait_for postgres 60 docker exec kanz-ci-postgres pg_isready -U kanz
wait_for redis    30 docker exec kanz-ci-redis redis-cli ping
wait_for nats     60 nats_ready
wait_for kafka   180 kafka_ready

# --- The application role ----------------------------------------------------
# Created NOSUPERUSER, then VERIFIED. Creating it correctly and checking it are
# different acts: a future edit here, a hand-patched local database, or a role
# that already existed with different attributes all produce a DSN that looks
# right and silently disables RLS.
docker exec kanz-ci-postgres psql -U kanz -d kanz -q -v ON_ERROR_STOP=1 \
  -c "CREATE ROLE kanzapp LOGIN PASSWORD 'kanz' NOSUPERUSER;" \
  -c "CREATE DATABASE kanzapp OWNER kanzapp;" >/dev/null
docker exec kanz-ci-postgres psql -U kanz -d kanzapp -q -v ON_ERROR_STOP=1 \
  -c "GRANT ALL ON SCHEMA public TO kanzapp;" >/dev/null

# A single deterministic token rather than two booleans: psql renders bool as
# `t`/`f` unquoted but `false` under ::text, so comparing the raw values invites
# a check that is really testing psql's formatting. An absent role yields an
# empty string, which is also not CONSTRAINED — so a missing role fails here too
# rather than sailing past a comparison that only ever saw the happy shape.
attrs="$(docker exec kanz-ci-postgres psql -U kanz -d kanz -tAc \
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
echo "  role kanzapp verified NOSUPERUSER / NOBYPASSRLS — RLS will be enforced"

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
  -e NATS_URL="nats://localhost:${NATS_PORT}" -e NATS_REPLICAS=1 \
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

  export TEST_POSTGRES_URL='postgres://kanzapp:kanz@localhost:${PG_PORT}/kanzapp?sslmode=disable'
  export TEST_NATS_URL='nats://localhost:${NATS_PORT}'
  export TEST_KAFKA_BROKERS='localhost:${KAFKA_PORT}'
  go test -p 1 ./...

ORDER WARNING — run any real migration BEFORE the suite, never after.
internal/migrate's tests share this database and DROP schema_migrations at
setup, leaving their own fixture rows behind. Run \`kanz-migrate\` afterwards and
it collides at version 1; run it before and the suite erases its ledger. CI is
only safe here because its OMS migrate step precedes \`go test\`. Tracked
separately — do not work around it by editing a migration.
EOF
