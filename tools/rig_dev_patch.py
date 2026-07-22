#!/usr/bin/env python3
"""Adapt a production manifest for the dev rig, on stdout.

Two rig-only transformations, both because the rig is deliberately unlike prod:

1. Vault CSI -> dev Secret. Production mounts DSNs and credentials from Vault via
   secrets-store.csi.k8s.io. The dev rig has no Vault (ONBOARD-M6), so each such
   volume becomes a reference to the committed rig-dev-secrets Secret.

2. imagePullPolicy -> IfNotPresent on every container. The images all carry the
   :latest tag, which Kubernetes defaults to imagePullPolicy: Always. The rig runs
   images that tools/rig-images.sh built locally and `kind load`ed into the node;
   there is no registry to pull ghcr.io/kanz-eng/* from (it is private and the
   node is unauthenticated), so Always guarantees ImagePullBackOff. Forcing
   IfNotPresent makes the loaded image authoritative. Production keeps Always so it
   really does pull from ghcr.io — that is why this belongs in the rig patch and
   not in the committed manifest. It also retires the hand-patch that did this on
   the rig on 2026-07-12 and recorded nowhere.

csi.spiffe.io volumes are deliberately LEFT ALONE: after `rig-apply.sh --spire`
the rig runs real SPIRE, so its SVIDs are genuine and stripping them would
reintroduce the very deviation this replaces.
"""
import sys
import yaml

VAULT_DRIVER = "secrets-store.csi.k8s.io"


def patch_pod_spec(spec, path):
    for vol in spec.get("volumes") or []:
        csi = vol.get("csi")
        if not (isinstance(csi, dict) and csi.get("driver") == VAULT_DRIVER):
            continue
        spc = (csi.get("volumeAttributes") or {}).get("secretProviderClass")
        if not spc:
            # Fail loudly. A Vault volume with no class named is a manifest we do
            # not understand, and guessing a Secret name here would produce a mount
            # that is present and empty — which surfaces as a confusing config error
            # at boot rather than as the missing secret it actually is.
            sys.exit(f"FATAL: {path}: volume {vol.get('name')!r} uses {VAULT_DRIVER} "
                     f"with no secretProviderClass; cannot choose a dev Secret for it")
        # The dev Secret shadows the SecretProviderClass name-for-name, which is
        # the convention postgres-dev.yaml already established with `oms-db`.
        vol.pop("csi")
        vol["secret"] = {"secretName": spc}

    # Every container runs a kind-loaded image; never let the node reach for ghcr.
    for key in ("initContainers", "containers"):
        for container in spec.get(key) or []:
            container["imagePullPolicy"] = "IfNotPresent"


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: rig_dev_patch.py <manifest.yaml>")

    with open(sys.argv[1]) as fh:
        docs = [d for d in yaml.safe_load_all(fh) if d]

    if not docs:
        sys.exit(f"FATAL: {sys.argv[1]} parsed to no documents")

    for doc in docs:
        # Deployment/StatefulSet/DaemonSet/Job and the Argo Rollout all carry the
        # pod spec at the same path.
        tmpl = (doc.get("spec") or {}).get("template") or {}
        if tmpl.get("spec"):
            patch_pod_spec(tmpl["spec"], sys.argv[1])

    yaml.safe_dump_all(docs, sys.stdout, default_flow_style=False)


if __name__ == "__main__":
    main()
