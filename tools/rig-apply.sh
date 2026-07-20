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
# registration.yaml declares a ClusterSPIFFEID custom resource, which does not
# exist as a kind until spire-controller-manager's CRDs are installed on the
# cluster (they ship with the controller-manager project, not with SPIRE
# itself). Without them, `kubectl apply -f registration.yaml` on a fresh
# cluster fails with `no matches for kind "ClusterSPIFFEID"` — the rig comes up
# with SPIRE running and zero registration entries, agents issue no SVIDs, and
# the whole exercise is silently useless. kanz/infra/security/spire/crds/
# vendors those CRDs (fetched at review time, not at apply time — the point of
# this script is that the rig rebuilds from the repo, and a network fetch
# mid-apply reintroduces the exact outside-the-repo dependency being removed).
# Their version is pinned to the spire-controller-manager image tag in
# spire-server.yaml; kanz/test/arch/spire_crd_version_test.go fails the build
# if the two drift apart.
#
# STAGES. --spire is the only stage this script implements today. A --deploy
# stage (rolling out the Kanz workloads themselves once SPIRE is live) lands
# in a follow-up task; the argument loop below is written so that stage drops
# in as another case arm and another "did" contribution, not a rewrite.
set -euo pipefail

CLUSTER="${CLUSTER:-kanz-dryrun}"
SPIRE_DIR="kanz/infra/security/spire"
CRD_DIR="$SPIRE_DIR/crds"

usage() { echo "usage: $0 [--spire] [--cluster NAME]" >&2; exit 2; }

# Fail closed if this isn't being run from the repo root: every path below is
# repo-relative, and a relative-path kubectl apply that silently no-ops (or
# applies the wrong file) is exactly the class of defect this rig has been
# built to stop tolerating. Checked lazily, from inside apply_spire, so that
# no-args and bad-flag invocations hit `usage` (exit 2, usage text) rather than
# this FATAL regardless of where they're run from — the same shape
# tools/rig-images.sh uses (its equivalent check lives inside matrix(), called
# only once an action needs the file it guards).
require_repo_root() {
  [ -d "$SPIRE_DIR" ] || {
    echo "FATAL: $SPIRE_DIR not found. Run this script from the repository root" >&2
    echo "(the directory containing kanz/, tools/, KANZ_TASKS.md)." >&2
    exit 2
  }
}

apply_spire() {
  require_repo_root
  echo "==> applying SPIRE from $SPIRE_DIR to context $KUBECTL_CONTEXT"

  # CRDs before anything else: registration.yaml is a ClusterSPIFFEID CR, and
  # applying a CR before its CRD exists is the one-shot failure this stage
  # exists to prevent. Loop over the vendor directory rather than naming a
  # single file so a future CRD (e.g. ClusterFederatedTrustDomain, if a
  # ClusterFederatedTrustDomain CR is ever added under registration.yaml)
  # is picked up without editing this script.
  for crd in "$CRD_DIR"/*.yaml; do
    kubectl --context "$KUBECTL_CONTEXT" apply -f "$crd"
  done
  # Applying a CR immediately after its CRD can race the API server's
  # discovery cache (the CRD is accepted but not yet Established, so the new
  # kind briefly still 404s) — wait for each one before moving on.
  for crd in "$CRD_DIR"/*.yaml; do
    name="$(awk '/^  name:/ { print $2; exit }' "$crd")"
    kubectl --context "$KUBECTL_CONTEXT" wait --for=condition=established --timeout=60s "crd/$name"
  done

  kubectl --context "$KUBECTL_CONTEXT" apply -f "$SPIRE_DIR/namespaces.yaml"
  kubectl --context "$KUBECTL_CONTEXT" apply -f "$SPIRE_DIR/rbac.yaml"
  kubectl --context "$KUBECTL_CONTEXT" apply -f "$SPIRE_DIR/spire-server.yaml"
  kubectl --context "$KUBECTL_CONTEXT" apply -f "$SPIRE_DIR/spire-agent.yaml"
  kubectl --context "$KUBECTL_CONTEXT" rollout status statefulset/spire-server -n spire-system --timeout=180s
  kubectl --context "$KUBECTL_CONTEXT" rollout status daemonset/spire-agent   -n spire-system --timeout=180s
  # ClusterSPIFFEID is cluster-scoped (no -n): applied last because the
  # controller-manager sidecar that consumes it only exists once spire-server
  # is up, and an agent with no registration entries issues no SVIDs — that
  # is SPIRE running and useless, not SPIRE working.
  kubectl --context "$KUBECTL_CONTEXT" apply -f "$SPIRE_DIR/registration.yaml"
  echo "==> SPIRE applied and rolled out"
}

[ $# -gt 0 ] || usage
want_spire=""
did=""
while [ $# -gt 0 ]; do
  case "$1" in
    # --spire only records intent here; it does not run yet. Every kubectl
    # call in apply_spire targets $KUBECTL_CONTEXT, which is derived from
    # $CLUSTER below — running immediately would use whatever $CLUSTER held
    # at this point in the loop, so `--spire --cluster NAME` (flag after
    # --spire) would silently apply against the wrong (default) cluster. All
    # flags are parsed first; the requested stage runs once parsing is done.
    --spire)   want_spire=1; did=1 ;;
    --cluster) shift; CLUSTER="${1:-}"; [ -n "$CLUSTER" ] || usage ;;
    *)         usage ;;
  esac
  shift
done
[ -n "$did" ] || usage

# kind contexts are named kind-<cluster>. Every kubectl invocation in this
# script is pinned to this context so `--cluster NAME` actually selects which
# cluster gets mutated, instead of silently acting on whatever context happens
# to be ambient (the defect this parameter previously had: parsed, validated,
# and then never read again).
KUBECTL_CONTEXT="kind-$CLUSTER"

[ -z "$want_spire" ] || apply_spire
