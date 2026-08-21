#!/usr/bin/env bash
# apply-rules.sh — the ONE place that decides which rule files Prometheus loads.
#
# WHY THIS IS A SCRIPT AND NOT A COMMENT (#230). It used to be a comment above
# the volume in prometheus.yaml:
#
#   kubectl -n kanz-observability create configmap slo-rules \
#     --from-file=.../slo/slo.recording.rules.yaml \
#     --from-file=.../slo/slo.alerts.rules.yaml
#
# Two hand-written filenames, in prose, as the actual deployment contract. A rule
# file added anywhere else was silently not in the ConfigMap, so it parsed in
# nobody's checkout, loaded in no cluster and fired never — while the repository
# looked like it had gained alerts. That is the failure #230 exists to fix,
# reproduced by the fix for it.
#
# `rule_files` in prometheus.yaml is /etc/prometheus/rules/*.rules.yaml — a MOUNT
# path, not a repo path. It matches whatever the ConfigMap happens to contain, so
# it can never catch an omission: a ConfigMap with two of three rule files is
# indistinguishable from a complete one.
#
# So the list is derived, not written. Every *.rules.yaml under this directory is
# included, and adding one anywhere in the tree is sufficient to deploy it.
#
# --list prints the files instead of applying, so CI stages exactly the set the
# cluster would load rather than a second glob that can drift from this one.
# test/arch/observability_rules_reachable_test.go fails the build if it does.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
namespace="${KANZ_OBSERVABILITY_NAMESPACE:-kanz-observability}"
configmap="prometheus-rules"

# -print0/-d '' so a path containing a space cannot split into two arguments and
# half-populate the ConfigMap — the same silent-partial failure in miniature.
rules=()
while IFS= read -r -d '' f; do rules+=("$f"); done \
  < <(find "$here" -type f -name '*.rules.yaml' -print0 | sort -z)

# A rule set that is empty is never what anybody meant, and `create configmap`
# with no --from-file succeeds and produces an EMPTY ConfigMap — which mounts
# clean and loads zero rules. Refuse instead.
if [ "${#rules[@]}" -eq 0 ]; then
  echo "apply-rules.sh: no *.rules.yaml found under ${here} — refusing to create an" >&2
  echo "  empty ${configmap} ConfigMap. An empty one mounts successfully and loads no" >&2
  echo "  rules, so Prometheus would come up green with every alert silently absent." >&2
  exit 1
fi

if [ "${1:-}" = "--list" ]; then
  printf '%s\n' "${rules[@]}"
  exit 0
fi

args=()
for f in "${rules[@]}"; do args+=(--from-file="$f"); done

# Recreate rather than patch: a rule file DELETED from the repo must disappear
# from the ConfigMap too, and `create --dry-run | apply` is the only form that
# removes keys. Patching would leave a retired alert firing forever.
emit() {
  kubectl -n "$namespace" create configmap "$configmap" "${args[@]}" \
    --dry-run=client -o yaml
}

# --emit prints the ConfigMap manifest instead of applying it, and is how
# rules-configmap.yaml is regenerated (#625):
#
#   ./apply-rules.sh --emit > rules-configmap.yaml
#
# THE APPLY PATH BELOW IS THE SAME FUNCTION PIPED TO kubectl, on purpose. Argo CD
# now delivers this tree, so the committed manifest is what reaches a cluster —
# and if emitting and applying were two `create configmap` invocations, they
# would be two derivations of the list, which is exactly the state #230 ended.
# One function, two destinations.
#
# test/arch/observability_rules_reachable_test.go compares the committed manifest
# against this directory's *.rules.yaml, so a rule file added without
# regenerating fails the build rather than loading nowhere.
if [ "${1:-}" = "--emit" ]; then
  emit
  exit 0
fi

emit | kubectl -n "$namespace" apply -f -

echo "applied ${configmap} to ${namespace} with ${#rules[@]} rule file(s):"
printf '  %s\n' "${rules[@]}"
