#!/usr/bin/env bash
# Verify the published listener from outside the broker's container.
set -euo pipefail
export MSYS2_ARG_CONV_EXCL='*'
port="${KAFKA_PORT:-29092}"
[[ "$port" != 9092 ]] || { echo 'This check requires a non-default port' >&2; exit 1; }
name="kanz-ci-kafka-check-$$-$RANDOM"
exports="$(KAFKA_PORT="$port" bash "$(dirname "$0")/kafka.sh" "$name")"
id=$(docker inspect -f '{{.Id}}' "$name")
trap 'docker rm -fv "$id" >/dev/null' EXIT
eval "$exports"
[[ "$TEST_KAFKA_BROKERS" == "127.0.0.1:$port" ]]
image=$(docker inspect -f '{{.Image}}' "$id")
rpk() {
  docker run --rm -i --network host --entrypoint rpk "$image" -X "brokers=$TEST_KAFKA_BROKERS" "$@"
}
# The metadata response is authoritative for subsequent client connections.
rpk cluster info --format json | python3 -c '
import json, sys
assert json.load(sys.stdin)["brokers"] == [{"id": 0, "host": "127.0.0.1", "port": int(sys.argv[1])}]
' "$port"
rpk topic create backing-probe --partitions 1 --replicas 1 >/dev/null
printf '%s\n' "$name" | rpk topic produce backing-probe >/dev/null
[[ "$(rpk topic consume backing-probe --num 1 --offset start --format '%v')" == "$name" ]]

# Both attempts use the same client and listener on this owned broker. The
# second phase changes only the dynamic broker policy.
implicit() {
  printf '%s\n' "$name" | rpk topic produce --allow-auto-topic-creation --delivery-timeout 6s implicit-probe
}
if implicit; then echo 'Undeclared topic accepted by the default backing broker' >&2; exit 1; fi
topic_names=$(rpk topic list --format json)
python3 -c 'import json,sys; assert "implicit-probe" not in [t["name"] for t in json.load(sys.stdin)]' <<<"$topic_names"
docker exec "$id" rpk cluster config set auto_create_topics_enabled true >/dev/null
for _ in $(seq 1 20); do
  [[ "$(docker exec "$id" rpk cluster config get auto_create_topics_enabled)" != true ]] || break
  sleep 1
done
[[ "$(docker exec "$id" rpk cluster config get auto_create_topics_enabled)" == true ]]
implicit >/dev/null
[[ "$(rpk topic consume implicit-probe --num 1 --offset start --format '%v')" == "$name" ]]
echo 'Verified explicit provisioning, disabled implicit creation, and enabled-policy control'
