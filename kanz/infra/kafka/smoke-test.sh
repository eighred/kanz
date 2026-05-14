#!/usr/bin/env bash
# Produce/consume smoke test for the Kanz Kafka durable log. Creates a throwaway
# topic, round-trips a uniquely-tagged message through it, then deletes it.
#
# Must run IN-CLUSTER — advertised listeners are in-cluster DNS, so a
# port-forward cannot complete a produce/consume. Run it as a one-off pod:
#   kubectl -n kanz-messaging run kafka-smoke --rm -i --restart=Never \
#     --image=apache/kafka:3.9.0 --command -- bash -c "$(cat smoke-test.sh)"
set -euo pipefail

BOOTSTRAP="${KAFKA_BOOTSTRAP:-kafka:9092}"
BIN="${KAFKA_BIN:-/opt/kafka/bin}"
TOPIC="smoke-test.$(date +%s)"
PAYLOAD="kanz-kafka-smoke-$(date +%s)-$$"

echo "== provisioned topics =="
"$BIN/kafka-topics.sh" --bootstrap-server "$BOOTSTRAP" --list

echo "== create $TOPIC =="
"$BIN/kafka-topics.sh" --bootstrap-server "$BOOTSTRAP" \
  --create --topic "$TOPIC" --partitions 1 --replication-factor 3

cleanup() {
  "$BIN/kafka-topics.sh" --bootstrap-server "$BOOTSTRAP" \
    --delete --topic "$TOPIC" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "== produce =="
printf 'smoke:%s\n' "$PAYLOAD" | "$BIN/kafka-console-producer.sh" \
  --bootstrap-server "$BOOTSTRAP" --topic "$TOPIC" \
  --property parse.key=true --property key.separator=:

echo "== consume =="
OUT="$("$BIN/kafka-console-consumer.sh" --bootstrap-server "$BOOTSTRAP" \
  --topic "$TOPIC" --from-beginning --timeout-ms 15000 \
  --property print.key=true 2>/dev/null || true)"

echo "$OUT" | grep -q "$PAYLOAD" \
  || { echo "FAIL: produced message not consumed"; echo "$OUT"; exit 1; }
echo "PASS: round-tripped message through $TOPIC"
echo "SMOKE TEST OK"
