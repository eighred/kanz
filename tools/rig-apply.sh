#!/usr/bin/env bash
# Stand the dev rig back up from the repository's own manifests.
#
# SPIRE has been declared in kanz/infra/security/spire/ all along and was never
# applied to the rig, which is why the rig's manifests were hand-stripped of
# their SPIFFE volumes: nobody could tell the difference between "SPIRE isn't
# needed here" and "SPIRE was declared but never stood up," and the rig drifted
# eight days from the repo as a result. Applying the in-repo manifests makes
# the rig's mTLS real instead of absent — this is not a dev deviation, it is
# running what the repo already declares.
#
# STAGES. --spire is the only stage this script implements today. A --deploy
# stage (rolling out the Kanz workloads themselves once SPIRE is live) lands
# in a follow-up task; the argument loop below is written so that stage drops
# in as another case arm and another "did" contribution, not a rewrite.
set -euo pipefail

CLUSTER="${CLUSTER:-kanz-dryrun}"
SPIRE_DIR="kanz/infra/security/spire"

usage() { echo "usage: $0 [--spire] [--cluster NAME]" >&2; exit 2; }

# Fail closed if this isn't being run from the repo root: every path below is
# repo-relative, and a relative-path kubectl apply that silently no-ops (or
# applies the wrong file) is exactly the class of defect this rig has been
# built to stop tolerating.
[ -d "$SPIRE_DIR" ] || {
  echo "FATAL: $SPIRE_DIR not found. Run this script from the repository root" >&2
  echo "(the directory containing kanz/, tools/, KANZ_TASKS.md)." >&2
  exit 2
}

apply_spire() {
  echo "==> applying SPIRE from $SPIRE_DIR"
  kubectl apply -f "$SPIRE_DIR/namespaces.yaml"
  kubectl apply -f "$SPIRE_DIR/rbac.yaml"
  kubectl apply -f "$SPIRE_DIR/spire-server.yaml"
  kubectl apply -f "$SPIRE_DIR/spire-agent.yaml"
  kubectl rollout status statefulset/spire-server -n spire-system --timeout=180s
  kubectl rollout status daemonset/spire-agent   -n spire-system --timeout=180s
  # ClusterSPIFFEID is cluster-scoped (no -n): applied last because the
  # controller-manager sidecar that consumes it only exists once spire-server
  # is up, and an agent with no registration entries issues no SVIDs — that
  # is SPIRE running and useless, not SPIRE working.
  kubectl apply -f "$SPIRE_DIR/registration.yaml"
  echo "==> SPIRE applied and rolled out"
}

[ $# -gt 0 ] || usage
did=""
while [ $# -gt 0 ]; do
  case "$1" in
    --spire)   apply_spire; did=1 ;;
    --cluster) shift; CLUSTER="${1:-}"; [ -n "$CLUSTER" ] || usage ;;
    *)         usage ;;
  esac
  shift
done
[ -n "$did" ] || usage
