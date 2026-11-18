#!/usr/bin/env bash
# The caller owns the healthy listener and retries the refused fixture after
# releasing the port. Creating a container may succeed before port publication.
set -euo pipefail
listener=$1
port=$2
name=$3
if failure=$(REDIS_PORT="$port" bash kanz/test/backing/valkey.sh "$name" 2>&1); then
  echo 'Fixture unexpectedly started on an occupied port' >&2
  exit 1
fi
grep -Eiq 'port is already allocated|address already in use' <<<"$failure"
[[ -z "$(docker ps -aq --filter "name=^/${name}$")" ]]
[[ "$(docker exec "$listener" valkey-cli --raw ping)" == PONG ]]
