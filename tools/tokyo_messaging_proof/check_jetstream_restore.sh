#!/usr/bin/env bash
# Exercise the account backup and isolated default-account restore used by the
# operator drill, with a second source account outside the backup's scope.
set -euo pipefail
export MSYS2_ARG_CONV_EXCL='*'
source "$(dirname "$0")/../testnet-data-plane-recovery.sh"
containers=()
cleanup() {
  local id
  for id in "${containers[@]}"; do docker rm -fv "$id" >/dev/null; done
}
trap cleanup EXIT
start() {
  local var=$1 id
  shift
  id=$(docker create "$@")
  containers+=("$id")
  docker start "$id" >/dev/null
  printf -v "$var" '%s' "$id"
}
start source_server --network none --tmpfs /data --entrypoint sh "$NATS_IMAGE" -ceu '
  cat > /tmp/nats.conf <<EOF
port: 4222
http_port: 8222
jetstream { store_dir: "/data" }
no_auth_user: fixture
accounts {
  __system__ { jetstream: enabled, users: [ { user: fixture } ] }
  other { jetstream: enabled, users: [ { user: other } ] }
}
EOF
  exec nats-server -c /tmp/nats.conf'
start target_server --network none --tmpfs /data "$NATS_IMAGE" --jetstream --store_dir=/data --http_port=8222
start source_cli --network "container:$source_server" "$NATS_BOX_IMAGE" sleep 300
start target_cli --network "container:$target_server" "$NATS_BOX_IMAGE" sleep 300
for pair in "$source_cli" "$target_cli"; do
  docker exec "$pair" sh -ceu 'for i in $(seq 1 20); do wget -qO- http://127.0.0.1:8222/healthz >/dev/null && exit; sleep 1; done; exit 1'
done
source_nats() { docker exec "$source_cli" nats --server nats://127.0.0.1:4222 "$@"; }
docker exec "$source_cli" nats --server nats://other:@127.0.0.1:4222 stream add UNRELATED --subjects other --storage file --defaults >/dev/null
source_nats stream add RECORDS --subjects records --storage file --defaults >/dev/null
source_nats stream add AUDIT --subjects audit --storage file --defaults >/dev/null
source_nats pub records first >/dev/null
source_nats pub records second >/dev/null
source_nats pub audit third >/dev/null
for durable in LEFT RIGHT; do
  source_nats consumer add RECORDS "$durable" --pull --ack explicit --defaults >/dev/null
done
source_nats consumer next RECORDS LEFT --ack --count 1 >/dev/null
source_nats account backup --check --critical-warnings --force /tmp/nats-backup >/dev/null
docker exec "$source_cli" tar -C /tmp -czf - nats-backup | docker exec -i "$target_cli" tar -C /tmp -xzf -
docker exec "$target_cli" nats --server nats://127.0.0.1:4222 account restore /tmp/nats-backup >/dev/null
jsz='http://127.0.0.1:8222/jsz?streams=true&consumers=true&config=true'
before=$(docker exec "$source_cli" wget -qO- "$jsz")
after=$(docker exec "$target_cli" wget -qO- "$jsz")
jq() { docker exec -i "$target_cli" jq "$@"; }
expected=$(nats_account_state __system__ <<<"$before")
actual=$(nats_account_state '$G' <<<"$after")
[[ "$expected" == "$actual" ]]
nats_restore_evidence <<<"$actual" | jq -e '.nats_streams==2 and .nats_consumers==2 and .nats_messages==3 and
  .nats_account_state=={format:"named-streams-consumer-state-v1",source:"__system__",restored:"$G"}' >/dev/null
jq -e 'map(.name)==["AUDIT","RECORDS"] and .[1].messages==2 and
  .[1].consumers[0].name=="LEFT" and .[1].consumers[0].ack_floor.stream_seq==1 and
  .[1].consumers[1].name=="RIGHT" and .[1].consumers[1].ack_floor.stream_seq==0' <<<"$actual" >/dev/null
if nats_account_state __system__ <<<"$after" >/dev/null 2>&1; then exit 1; fi
swapped=$(jq '(.account_details[].stream_detail[] | select(.name=="RECORDS") |
  .consumer_detail[]) |= (if .name=="LEFT" then .name="RIGHT" else .name="LEFT" end |
    .config.durable_name=.name | .config.name=.name)' <<<"$after")
[[ "$(nats_account_state '$G' <<<"$swapped")" != "$expected" ]]
swapped=$(jq '(.account_details[].stream_detail[].state.messages) |= (if .==2 then 1 else 2 end)' <<<"$after")
[[ "$(nats_account_state '$G' <<<"$swapped")" != "$expected" ]]
missing=$(jq 'del(.account_details[].stream_detail[].consumer_detail[]?.ack_floor)' <<<"$after")
if nats_account_state '$G' <<<"$missing" >/dev/null 2>&1; then exit 1; fi

for elapsed in 0 23; do
  receipt=$(emit_restore_receipt fixture-key fixture-sha fixture-release "$elapsed" 8 \
    "$(nats_restore_evidence <<<"$actual")" '{"redis_keys":0}')
  jq -e --argjson elapsed "$elapsed" '.format=="kanz-testnet-restore-drill-v1" and
    .status=="RESTORE_VERIFIED" and .drill_elapsed_seconds==$elapsed and
    (.operational_recovery | .state=="UNKNOWN" and has("rpo_seconds") and has("rto_seconds") and
       .rpo_seconds==null and .rto_seconds==null) and
    (has("rpo_seconds") or has("rto_seconds") or has("verified_at") | not) and
    .nats_streams==2 and .redis_keys==0' <<<"$receipt" >/dev/null
done
