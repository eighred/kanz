#!/usr/bin/env bash
# Publish one Kafka-compatible development listener on the host loopback.
set -euo pipefail
port="${KAFKA_PORT:-9092}"
name="${1:-kanz-ci-kafka}"
image='docker.redpanda.com/redpandadata/redpanda:v26.2.6@sha256:8b1076e0d61bfceb66594dc01536427ce0b529f8ead35fdab9763ab6085891ce'
[[ "$port" =~ ^[1-9][0-9]{0,4}$ && "$port" -le 65535 ]] || { echo 'Invalid KAFKA_PORT' >&2; exit 1; }
[[ "$name" =~ ^kanz-ci-kafka(-[a-z0-9-]+)?$ ]] || { echo 'Invalid fixture name' >&2; exit 1; }
endpoint="127.0.0.1:$port"
id="$(docker create --label "io.kanz.backing.run=${KANZ_BACKING_RUN:-standalone}" --name "$name" -p "$endpoint:19092" "$image" redpanda start \
  --smp=1 --memory=1G --reserve-memory=0M --overprovisioned --check=false \
  --unsafe-bypass-fsync=false --node-id=0 \
  --kafka-addr=internal://0.0.0.0:9092,external://0.0.0.0:19092 \
  "--advertise-kafka-addr=internal://127.0.0.1:9092,external://$endpoint" \
  --set redpanda.auto_create_topics_enabled=false --set redpanda.default_topic_replications=1 \
  --set redpanda.write_caching_default=false)"
trap 'docker rm -fv "$id" >/dev/null' EXIT
docker start "$id" >/dev/null
for _ in $(seq 1 60); do
  if docker exec "$id" rpk cluster info -X brokers=127.0.0.1:9092 >/dev/null 2>&1; then
    [[ "$(docker port "$id" 19092/tcp)" == "$endpoint" ]]
    printf "export TEST_KAFKA_BROKERS='%s'\n" "$endpoint"
    trap - EXIT
    exit 0
  fi
  sleep 1
done
echo 'Local Redpanda did not become ready' >&2
exit 1
