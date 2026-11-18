#!/usr/bin/env bash
# Drive the top-level entry point through a late refusal and successful retry.
set -euo pipefail
export MSYS2_ARG_CONV_EXCL='*'
export PG_PORT=${PG_PORT:-35141} NATS_PORT=${NATS_PORT:-35142} REDIS_PORT=${REDIS_PORT:-35143} KAFKA_PORT=${KAFKA_PORT:-35144}
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
if bash "$dir/up.sh" "$suffix"; then echo 'Occupied Kafka name was accepted' >&2; exit 1; fi
[[ "$(docker inspect --format '{{.Id}}' "${names[3]}")" == "$foreign" ]]
for name in "${names[@]:0:3}"; do
  [[ -z "$(docker ps -aq --filter "name=^/${name}$")" ]]
done
docker rm -fv "$foreign" >/dev/null
foreign=''
result=$(bash "$dir/up.sh" "$suffix")
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
printf '%s\n' 'Verified late collision, preserved foreign container, rolled back partial stack and retried all four services'
