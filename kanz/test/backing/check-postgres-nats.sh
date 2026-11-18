#!/usr/bin/env bash
# Only start fresh, uniquely named local services and retire their actual IDs.
set -euo pipefail
export MSYS2_ARG_CONV_EXCL='*'
suffix="-check-$$-$RANDOM"
pg_port=${PG_PORT:-25432}
nats_port=${NATS_PORT:-24222}
[[ "$pg_port" != 5432 && "$nats_port" != 4222 ]]
exports=$(PG_PORT=$pg_port NATS_PORT=$nats_port bash "$(dirname "$0")/postgres-nats.sh" "$suffix")
pg=$(docker inspect -f '{{.Id}}' "kanz-ci-postgres$suffix")
nats=$(docker inspect -f '{{.Id}}' "kanz-ci-nats$suffix")
trap 'docker rm -fv "$pg" "$nats" >/dev/null' EXIT
eval "$exports"
[[ "$(docker port "$pg" 5432/tcp)" == "127.0.0.1:$pg_port" ]]
[[ "$(docker port "$nats" 4222/tcp)" == "127.0.0.1:$nats_port" ]]
[[ "$TEST_POSTGRES_URL" == "postgres://kanzapp:kanz@127.0.0.1:$pg_port/kanzapp?sslmode=disable" ]]
[[ "$TEST_NATS_URL" == "nats://127.0.0.1:$nats_port" ]]
client=$(docker inspect -f '{{.Image}}' "$pg")
role=$(docker run --rm --network host "$client" psql "$TEST_POSTGRES_URL" -AtX -v ON_ERROR_STOP=1 \
  -c 'select current_database(), current_user, rolsuper, rolbypassrls from pg_roles where rolname=current_user')
[[ "$role" == 'kanzapp|kanzapp|f|f' ]]
box='natsio/nats-box@sha256:ffce8bd103383f179f8c7f11cf645726acf5d17280706c530c3b342dbe16334c'
cli() { docker run --rm --network host "$box" nats --server "$TEST_NATS_URL" "$@"; }
cli stream add BACKING --subjects fixture.sample --storage file --defaults >/dev/null
cli pub fixture.sample "fixture$suffix" >/dev/null
cli stream get BACKING 1 --json | python3 -c '
import base64, json, sys
record = json.load(sys.stdin)
if record["seq"] != 1 or record["subject"] != "fixture.sample" or base64.b64decode(record["data"], validate=True) != sys.argv[1].encode():
    sys.exit("Published message differs from the stored record")
' "fixture$suffix"
