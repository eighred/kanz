#!/usr/bin/env bash
set -euo pipefail
export MSYS2_ARG_CONV_EXCL='*'
source "$(dirname "$0")/../testnet-data-plane-recovery.sh"
id=$(docker create --network none --read-only --user 999:999 --tmpfs /data:uid=999,gid=999,mode=0700 \
  --tmpfs /run/secrets/redis:uid=999,gid=999,mode=0700 \
  --entrypoint sh "$REDIS_IMAGE" -ceu '
    umask 077
    cat /proc/sys/kernel/random/uuid > /run/secrets/redis/password
    printf "requirepass %s\n" "$(cat /run/secrets/redis/password)" > /run/secrets/redis/fixture.conf
    exec redis-server /run/secrets/redis/fixture.conf --dir /data --save ""')
trap 'docker rm -fv "$id" >/dev/null' EXIT
docker start "$id" >/dev/null
k() {
  [[ "${*:1:7}" == '-n kanz-messaging exec redis-0 -c redis --' ]] || return 1
  shift 7
  docker exec "$id" "$@"
}
redis() {
  docker exec "$id" sh -ceu 'export REDISCLI_AUTH="$(cat /run/secrets/redis/password)"; exec redis-cli --raw "$@"' _ "$@"
}
for _ in $(seq 1 30); do [[ "$(redis PING 2>/dev/null)" == PONG ]] && break; sleep 1; done
[[ "$(redis SET first retained)" == OK ]]
result=$(redis_backup_checkpoint)
[[ "$result" =~ ^1$'\n'[1-9][0-9]*$ ]]
docker exec "$id" test -s /data/dump.rdb
[[ "$(redis SET second unsaved)" == OK ]]
for denied in save dbsize lastsave; do
  [[ "$(redis ACL SETUSER default "-$denied")" == OK ]]
  if result=$(redis_backup_checkpoint); then echo "Missing $denied accepted" >&2; exit 1; fi
  [[ -z "$result" ]]
  [[ "$(redis ACL SETUSER default "+$denied")" == OK ]]
done
result=$(redis_backup_checkpoint)
[[ "$result" =~ ^2$'\n'[1-9][0-9]*$ ]]
docker exec "$id" sh -ceu 'cat /proc/sys/kernel/random/uuid > /run/secrets/redis/password'
if result=$(redis_backup_checkpoint); then echo 'Unauthenticated checkpoint accepted' >&2; exit 1; fi
[[ -z "$result" ]]
