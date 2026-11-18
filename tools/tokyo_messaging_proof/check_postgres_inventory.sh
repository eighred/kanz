#!/usr/bin/env bash
set -eu
source "$(dirname "$0")/../testnet-data-plane-recovery.sh"
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
make_inventory() {
  local db
  : >"$work/source.tsv"
  for db in "$@"; do
    printf '%s|3|12|0|0/1A2F\n' "$db" >>"$work/source.tsv"
    printf 'PGDMP\001' >"$work/$db.dump"
  done
}
refuses() {
  if (verify_postgres_inventory "$work" "$@") >/dev/null 2>&1; then
    echo 'Incomplete database archive was accepted' >&2; exit 1
  fi
}
for tv in off qualify; do
  dbs=("${DATABASES[@]}")
  [[ "$tv" == off ]] || dbs+=(tv_sync)
  make_inventory "${dbs[@]}"
  verify_postgres_inventory "$work" "${dbs[@]}"
  cp "$work/source.tsv" "$work/full"
  sed '1d' "$work/full" >"$work/source.tsv"; refuses "${dbs[@]}"
  cat "$work/full" "$work/full" >"$work/source.tsv"; refuses "${dbs[@]}"
  sed '1s/|0|/|1|/' "$work/full" >"$work/source.tsv"; refuses "${dbs[@]}"
  printf '%s' "$(cat "$work/full")" >"$work/source.tsv"; refuses "${dbs[@]}"
  cp "$work/full" "$work/source.tsv"
  touch "$work/unknown.dump"; refuses "${dbs[@]}"; rm "$work/unknown.dump"
  rm "$work/audit.dump"; refuses "${dbs[@]}"
  : >"$work/audit.dump"; refuses "${dbs[@]}"
  printf plaintext >"$work/audit.dump"; refuses "${dbs[@]}"
done
