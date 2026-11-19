#!/usr/bin/env bash
# Drive the top-level entry point through a late refusal and successful retry.
set -euo pipefail
export MSYS2_ARG_CONV_EXCL='*'
requested=("${PG_PORT:-}" "${NATS_PORT:-}" "${REDIS_PORT:-}" "${KAFKA_PORT:-}")
export PG_PORT NATS_PORT REDIS_PORT KAFKA_PORT
dir=$(cd "$(dirname "$0")" && pwd)
suffix="-check-$(python3 -c 'import uuid; print(uuid.uuid4().hex)')"
names=()
for service in postgres nats redis kafka; do names+=("kanz-ci-$service$suffix"); done
image='nats@sha256:d4ac35882ac65aff236cd65b9d3fa4d24332c681e1a85f94eedccd3cdd65b1da'
foreign=$(docker create --network none --name "${names[3]}" "$image")
owned=()
cleanup() {
  if [[ -n "$foreign" ]]; then docker rm -fv "$foreign" >/dev/null; fi
  if ((${#owned[@]})); then docker rm -fv "${owned[@]}" >/dev/null; fi
}
trap cleanup EXIT
start_stack() {
  local ports attempt index retry
  local -a selected
  for attempt in 1 2 3; do
    # Keep all four sockets open during selection so the OS returns distinct
    # ports. Bind explicit requests first, leaving their values unchanged.
    ports=$(python3 - "${requested[@]}" <<'PY'
from contextlib import ExitStack
import re
import socket
import sys

requested = sys.argv[1:]
selected = [0] * len(requested)
with ExitStack() as sockets:
    for i in sorted(range(len(requested)), key=lambda i: not requested[i]):
        text = requested[i]
        if text and (not re.fullmatch(r"[1-9][0-9]{0,4}", text) or int(text) > 65535):
            sys.exit("Invalid requested fixture port")
        port = int(text or 0)
        sock = sockets.enter_context(socket.socket())
        try:
            sock.bind(("127.0.0.1", port))
        except OSError as exc:
            sys.exit(f"Cannot bind fixture listener 127.0.0.1:{port}: {exc}")
        selected[i] = sock.getsockname()[1]
    print(*selected)
PY
    ) || return 1
    read -ra selected <<<"$ports"
    PG_PORT=${selected[0]} NATS_PORT=${selected[1]} REDIS_PORT=${selected[2]} KAFKA_PORT=${selected[3]}
    if result=$(bash "$dir/up.sh" "$suffix" 2>&1); then return 0; fi
    # Docker binds after the sockets close. Only a competing listener on an
    # automatically chosen port warrants selecting a new set and retrying.
    retry=false
    if [[ "$result" == *'address already in use'* || "$result" == *'port is already allocated'* ]]; then
      for index in "${!selected[@]}"; do
        if [[ -z "${requested[index]}" && "$result" =~ 127\.0\.0\.1:${selected[index]}([^0-9]|$) ]]; then retry=true; fi
      done
    fi
    [[ "$retry" == true ]] || break
  done
  printf '%s\n' "$result" >&2
  return 1
}
result=''
if start_stack; then echo 'Occupied Kafka name was accepted' >&2; exit 1; fi
[[ "$result" == *"container name \"/${names[3]}\" is already in use by container \"$foreign\""* ]] || {
  echo 'Startup failed before the intended Kafka name collision' >&2; exit 1;
}
[[ "$(docker inspect --format '{{.Id}}' "${names[3]}")" == "$foreign" ]]
for name in "${names[@]:0:3}"; do
  [[ -z "$(docker ps -aq --filter "name=^/${name}$")" ]]
done
docker rm -fv "$foreign" >/dev/null
foreign=''
start_stack
read -ra owned <<<"$(sed -n 's/^  Tear down this stack: docker rm -fv //p' <<<"$result")"
[[ ${#owned[@]} == 4 ]]
for id in "${owned[@]}"; do [[ "$id" =~ ^[a-f0-9]{64}$ ]]; done
for name in "${names[@]}"; do
  current=$(docker inspect --format '{{.Id}}' "$name")
  [[ " ${owned[*]} " == *" $current "* ]]
done
for endpoint in "127.0.0.1:$PG_PORT/" "127.0.0.1:$REDIS_PORT/" "127.0.0.1:$NATS_PORT'" "127.0.0.1:$KAFKA_PORT'"; do
  [[ "$result" == *"$endpoint"* ]]
done
grep -Fq "export TEST_ACCOUNTING_MIGRATION_POSTGRES_URL='postgres://kanzapp:kanz@127.0.0.1:$PG_PORT/kanz_accounting_migration?sslmode=disable'" <<<"$result"
printf '%s\n' 'Verified late collision, preserved foreign container, rolled back partial stack and retried all four services'
