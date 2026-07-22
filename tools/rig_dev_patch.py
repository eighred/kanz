#!/usr/bin/env python3
"""Adapt a production manifest for the dev rig, on stdout.

Three rig-only transformations, each because the rig is deliberately unlike prod
(Vault CSI -> dev Secret; :latest -> IfNotPresent; SPIFFE bus mesh -> plaintext,
since the rig's NATS is dev-plaintext). See the inline comments for each:

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

# Env vars that point a workload at production-only infrastructure the rig does
# not run. Removed so the workload takes its in-process / plaintext fallback.
_DROP_ENV_EXACT = frozenset({"OMS_VENUE_ENDPOINTS"})


def _drop_on_rig(name):
    if name in _DROP_ENV_EXACT:
        return True
    # Any SPIFFE workload-API socket (SPIFFE_ENDPOINT_SOCKET, *_SPIFFE_SOCKET, ...).
    return "SPIFFE" in name and "SOCKET" in name


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

    for key in ("initContainers", "containers"):
        for container in spec.get(key) or []:
            # Every container runs a kind-loaded image; never reach for ghcr.
            container["imagePullPolicy"] = "IfNotPresent"

            # Strip the env vars that wire this workload to production-only
            # infrastructure the rig deliberately does not run, so it falls back to
            # the in-process / plaintext path instead of crash-looping on a dial
            # that can never succeed here:
            #
            #  * SPIFFE workload-API socket (any *SPIFFE*SOCKET*). With it set the
            #    transport mesh comes up (mesh.Enabled()) and the workload dials NATS
            #    over mTLS; the rig's NATS is the declared dev-plaintext deviation
            #    (infra/nats/bootstrap-job-dev-plaintext.yaml), so the dial fails
            #    "nats: secure connection not available". Dropping the socket leaves
            #    the mesh disabled (transport.NewMesh("").Enabled() == false) and the
            #    workload uses the same plaintext bus the other loop services do. SPIRE
            #    still issues the pod an SVID; it is simply unused until NATS speaks mTLS.
            #
            #  * OMS_VENUE_ENDPOINTS. INFRA-M7a made every venue an out-of-process
            #    adapter (venue-binance/venue-okx), and those are NOT on the rig — they
            #    need real exchange credentials the rig must never hold. An empty value
            #    makes the OMS use its in-process simulator on OMS_SIM_VENUE_MIC (XSIM)
            #    instead of dialing venue-*.svc:9000 and failing "connection refused".
            #    Orders on the rig target XSIM.
            env = container.get("env")
            if env:
                container["env"] = [e for e in env if not _drop_on_rig(e.get("name", ""))]


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
