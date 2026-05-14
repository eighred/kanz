#!/usr/bin/env sh
# Pub/sub smoke test for the Kanz NATS JetStream backbone. Confirms a live
# subscriber receives a published message and that it was persisted to the
# MARKET stream, then cleans up after itself.
#
# Usage (needs the `nats` CLI and reachability to NATS):
#   kubectl -n kanz-messaging port-forward svc/nats 4222:4222 &
#   NATS_URL=nats://localhost:4222 ./smoke-test.sh
set -eu

NATS_URL="${NATS_URL:-nats://localhost:4222}"
SUBJECT="market.smoketest.ping"
PAYLOAD="kanz-nats-smoke-$(date +%s)"

echo "== provisioned streams =="
nats --server "$NATS_URL" stream ls

echo "== subscribe market.> (background) =="
OUT="$(mktemp)"
nats --server "$NATS_URL" sub "market.>" --count=1 --timeout=10s >"$OUT" 2>&1 &
SUB_PID=$!
sleep 1

echo "== publish $SUBJECT =="
nats --server "$NATS_URL" pub "$SUBJECT" "$PAYLOAD"

wait "$SUB_PID" || { echo "FAIL: live subscriber received nothing"; cat "$OUT"; exit 1; }
grep -q "$PAYLOAD" "$OUT" || { echo "FAIL: payload mismatch"; cat "$OUT"; exit 1; }
echo "PASS: live subscriber received the message"

echo "== verify persisted to MARKET stream =="
nats --server "$NATS_URL" stream subjects MARKET "$SUBJECT" | grep -q "$SUBJECT" \
  || { echo "FAIL: message not persisted to MARKET"; exit 1; }
echo "PASS: message persisted to MARKET stream"

echo "== cleanup =="
nats --server "$NATS_URL" stream purge MARKET --subject "market.smoketest.>" -f
rm -f "$OUT"
echo "SMOKE TEST OK"
