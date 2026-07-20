#!/usr/bin/env bash
# Rebuild the dev rig's images from source and load them into kind.
#
# The rig's images were built by hand on 2026-07-12 and recorded nowhere, so the
# cluster drifted eight days from the repository and nothing could bring it back.
# That is the third instance of the same defect this week (the NATS bootstrap and
# the Postgres role were the first two): a one-shot setup step with no re-run path.
#
# THE SERVICE LIST IS NOT DUPLICATED HERE. It is parsed out of the CI build
# matrix, which test/arch/deployability_test.go already forces to stay complete.
# A local list would be a second enumeration that drifts silently; parsing makes
# the drift unrepresentable rather than merely detectable.
set -euo pipefail

WORKFLOW="${WORKFLOW:-.github/workflows/build.yml}"
REGISTRY="${REGISTRY:-ghcr.io/kanz-eng}"
CLUSTER="${CLUSTER:-kanz-dryrun}"
MIN_SERVICES=20   # fail-closed floor; the matrix had 23 entries on 2026-07-20

usage() { echo "usage: $0 [--list|--build|--load] [--cluster NAME]" >&2; exit 2; }

matrix() {
  [ -f "$WORKFLOW" ] || { echo "FATAL: $WORKFLOW not found (run from the repo root)" >&2; exit 2; }
  awk '
    /^[[:space:]]*-[[:space:]]+service:[[:space:]]*/  { svc=$NF; next }
    /^[[:space:]]*dockerfile:[[:space:]]*/ && svc!="" { print svc, $NF; svc="" }
  ' "$WORKFLOW"
}

require_matrix() {
  local n; n=$(matrix | wc -l)
  if [ "$n" -lt "$MIN_SERVICES" ]; then
    echo "FATAL: parsed only $n services from $WORKFLOW (expected >= $MIN_SERVICES)." >&2
    echo "The matrix format changed and this parser no longer understands it. Teach it the" >&2
    echo "new shape — do NOT lower MIN_SERVICES. A rebuild that silently skips services" >&2
    echo "produces a rig that is stale in exactly the way this tool exists to prevent." >&2
    exit 1
  fi
}

generate_sdk() {
  echo "==> regenerating kanz-schemas Go SDK (generated-not-committed, EVT-15a)"
  ( cd kanz-schemas && buf generate )
}

build() {
  require_matrix; generate_sdk
  while read -r svc dockerfile; do
    echo "==> build $REGISTRY/$svc:latest  (-f $dockerfile)"
    docker build -f "$dockerfile" -t "$REGISTRY/$svc:latest" .
  done < <(matrix)
}

load() {
  require_matrix
  while read -r svc _; do
    echo "==> kind load $REGISTRY/$svc:latest -> $CLUSTER"
    kind load docker-image "$REGISTRY/$svc:latest" --name "$CLUSTER"
  done < <(matrix)
}

[ $# -gt 0 ] || usage
did=""
while [ $# -gt 0 ]; do
  case "$1" in
    --list)    require_matrix; matrix; did=1 ;;
    --build)   build; did=1 ;;
    --load)    load;  did=1 ;;
    --cluster) shift; CLUSTER="${1:-}"; [ -n "$CLUSTER" ] || usage ;;
    *)         usage ;;
  esac
  shift
done
[ -n "$did" ] || usage
