#!/usr/bin/env bash
# Start the local redis-tag test dependency. Emit exports only after verifying
# the server behind the published port and its protocol identity.
set -euo pipefail

port="${REDIS_PORT:-6379}"
name="${1:-kanz-ci-redis}"
image='valkey/valkey:9.1.3-alpine@sha256:bf09d58e96d6b4524a15396b65b242bbe18beebbcc15a04d8ee75ddac62fae0b'
version='9.1.3'
[[ "$port" =~ ^[1-9][0-9]{0,4}$ && "$port" -le 65535 ]] || { echo 'Invalid REDIS_PORT' >&2; exit 1; }
[[ "$name" =~ ^kanz-ci-redis(-[a-z0-9-]+)?$ ]] || { echo 'Invalid fixture name' >&2; exit 1; }
id="$(docker create --name "$name" -p "127.0.0.1:${port}:6379" "$image")"
trap 'docker rm -fv "$id" >/dev/null' EXIT
docker start "$id" >/dev/null
ready=false
for _ in $(seq 1 30); do
  if [[ "$(docker exec "$id" valkey-cli --raw ping)" == PONG ]]; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  echo 'Local Valkey did not become ready' >&2
  exit 1
fi
identity="$(docker exec "$id" valkey-cli --raw INFO server | tr -d '\r')"
grep -Fxq 'server_name:valkey' <<< "$identity"
grep -Fxq "valkey_version:$version" <<< "$identity"
[[ "$(docker port "$id" 6379/tcp)" == "127.0.0.1:$port" ]]
printf "export TEST_REDIS_URL='redis://127.0.0.1:%s/0'\nexport TEST_VALKEY_VERSION='%s'\n" "$port" "$version"
trap - EXIT
