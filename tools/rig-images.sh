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
#
# PARSER CONTRACT (review fix, 2026-07-20). The naive version of this parser
# required `service:` to appear before `dockerfile:` in each matrix entry, read
# values as the last whitespace-separated token ($NF), and floored the row COUNT
# only. All three properties are broken:
#   - a YAML formatter alphabetizing keys ("dockerfile" < "service") silently
#     drops the entry, with the row count unaffected;
#   - an inline `# comment` after a value corrupts the value (`$NF` captures the
#     comment text, not the value), also with the row count unaffected.
# A floor over the row count cannot catch either, because neither changes the
# count. So the parser now (1) strips trailing comments before extracting
# anything, (2) extracts by splitting on the key name rather than by position,
# so key order within an entry is irrelevant, and (3) tracks entry boundaries
# explicitly and treats a `service:` with no `dockerfile:` (or vice versa) as a
# hard parse failure naming the offending entry. The row-count floor
# below is kept as a backstop, not the primary defense.
set -euo pipefail

WORKFLOW="${WORKFLOW:-.github/workflows/build.yml}"
REGISTRY="${REGISTRY:-ghcr.io/kanz-eng}"
CLUSTER="${CLUSTER:-kanz-dryrun}"
MIN_SERVICES=20   # fail-closed floor; the matrix had 23 entries on 2026-07-20

usage() { echo "usage: $0 [--list|--build|--load] [--cluster NAME]" >&2; exit 2; }

# MATRIX_CACHE memoizes the parsed rows for the lifetime of the process, so a
# script invocation that needs both the row count (require_matrix) and the
# rows themselves (list/build/load) parses build.yml exactly once.
MATRIX_LOADED=0
MATRIX_CACHE=""

matrix() {
  if [ "$MATRIX_LOADED" -eq 0 ]; then
    [ -f "$WORKFLOW" ] || { echo "FATAL: $WORKFLOW not found (run from the repo root)" >&2; exit 2; }
    # One matrix entry is a YAML mapping that starts with `- ` on either its
    # service: or dockerfile: line (whichever comes first) and continues with
    # plain-indented keys. We track entry boundaries explicitly rather than
    # assuming a fixed key order, strip trailing `#` comments before pulling
    # values out (so an inline comment cannot corrupt a value), and flush
    # (validate + emit) an entry the moment the next one starts or the file
    # ends. A flushed entry missing either key is a hard error naming the
    # entry's line range — silently omitting it would defeat the entire point
    # of parsing the matrix instead of hand-maintaining a list.
    MATRIX_CACHE="$(awk '
      function flush(endline) {
        # awk re-runs the END action after any exit(), even one called from a
        # main-block action (POSIX: exit still executes END unless already in
        # it). Once we have failed, later flush() calls (including the one
        # END makes on its own) must be no-ops so the FATAL line prints once.
        if (failed) return
        if (started) {
          if (svc == "" || dock == "") {
            printf("FATAL: malformed matrix entry (%s:%d-%d): service=%s dockerfile=%s\n", \
                   FILENAME, startline, endline, \
                   (svc == "" ? "<missing>" : svc), (dock == "" ? "<missing>" : dock)) > "/dev/stderr"
            failed = 1
            exit 1
          }
          print svc, dock
        }
        started = 0; svc = ""; dock = ""
      }
      {
        line = $0
        sub(/#.*/, "", line)                       # strip trailing comments before parsing
        is_new = (line ~ /^[[:space:]]*-[[:space:]]+(service|dockerfile):/)
        is_kv  = (line ~ /^[[:space:]]*-?[[:space:]]*(service|dockerfile):/)
        if (is_new) { flush(NR - 1); started = 1; startline = NR }
        if (is_kv) {
          tmp = line
          sub(/^[[:space:]]*-?[[:space:]]*/, "", tmp)   # drop leading dash/indent
          key = tmp
          sub(/:.*/, "", key)
          val = tmp
          sub(/^[^:]*:[[:space:]]*/, "", val)
          gsub(/[[:space:]]+$/, "", val)
          if (key == "service")        svc  = val
          else if (key == "dockerfile") dock = val
        }
      }
      END { flush(NR) }
    ' "$WORKFLOW")"
    MATRIX_LOADED=1
  fi
  [ -z "$MATRIX_CACHE" ] || printf '%s\n' "$MATRIX_CACHE"
}

require_matrix() {
  matrix >/dev/null
  # Count rows with awk's own numeric coercion (END {print c+0}) rather than
  # piping through external wc -l and string-comparing — immune to whitespace
  # padding by construction.
  local n
  n=$(printf '%s' "$MATRIX_CACHE" | awk 'NF { c++ } END { print c+0 }')
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
