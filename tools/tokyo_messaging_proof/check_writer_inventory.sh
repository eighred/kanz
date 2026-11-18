#!/usr/bin/env bash
# All cluster calls are fixtures; no backup body runs past the writer gate.
set -euo pipefail
source "$(dirname "$0")/../testnet-data-plane-recovery.sh"
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
ns_record='{"kind":"Namespace","metadata":{"name":"kanz-services","uid":"n"},"status":{"phase":"Active"}}'
pod='{"kind":"Pod","metadata":{"name":"p","namespace":"kanz-services","uid":"u"},"status":{"phase":"Succeeded"}}'
result() { printf '%s\n' "$1"; }
k() {
  case "$*" in
    'get namespace kanz-services --ignore-not-found -o json')
      [[ "$scenario" != namespace-error ]] || return 1
      if [[ "$scenario" == missing ]]; then return 0; fi
      if [[ "$scenario" == wrong-namespace ]]; then result '{}'; else result "$ns_record"; fi ;;
    '-n kanz-services get pods -o json')
      case "$scenario" in
        pods-error) return 1 ;;
        incomplete) result '{"kind":"List"}' ;;
        paginated) result '{"kind":"List","metadata":{"continue":"next"},"items":[]}' ;;
        untyped) result '{"kind":"List","items":[{}]}' ;;
        active) result "{\"kind\":\"List\",\"items\":[${pod/Succeeded/Running}]}" ;;
        terminal) result "{\"kind\":\"List\",\"items\":[$pod]}" ;;
        *) result '{"kind":"List","items":[]}' ;;
      esac ;;
    *) touch "$work/past-gate"; return 1 ;;
  esac
}
require_boundary() { :; }
for scenario in missing empty terminal; do check_writer_namespace kanz-services; done
for scenario in namespace-error wrong-namespace pods-error incomplete paginated untyped active; do
  if (backup) >"$work/out" 2>&1; then echo "Accepted $scenario" >&2; exit 1; fi
  grep -q '^recovery-refused:' "$work/out"
  [[ ! -e "$work/past-gate" ]]
done
echo 'Writer preflight accepted three bounded inventories and refused seven incomplete or active inventories before backup'
