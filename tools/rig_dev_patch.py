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
import os
import sys
import yaml

VAULT_DRIVER = "secrets-store.csi.k8s.io"

# Env vars that point a workload at production-only infrastructure the rig does
# not run. Removed so the workload takes its in-process / plaintext fallback.
_DROP_ENV_EXACT = frozenset({"OMS_VENUE_ENDPOINTS", "API_GATEWAY_OIDC_ISSUER"})

# Env vars the rig must ADD, keyed by manifest filename. The production manifest
# has no reason to carry these — they exist only because the rig substitutes a
# dev mechanism for one it cannot reach.
_ADD_ENV = {
    "api-gateway-deploy.yaml": [
        # The HS256 dev validator's key. api-gateway picks OIDC over HS256 when
        # API_GATEWAY_OIDC_ISSUER is set (cmd/api-gateway/main.go), which is why
        # that var is dropped above — the rig cannot reach login.eighred.com, so
        # with OIDC selected the gateway can authenticate nobody and its order
        # routes are untestable here. The key is read from the api-gateway-secrets
        # mount the manifest already declares, so this adds no new volume.
        {"name": "API_GATEWAY_JWT_SECRET_FILE",
         "value": "/run/secrets/gateway/jwt-secret"},
    ],
    "operator-deploy.yaml": [
        # The generic `imagePullPolicy: IfNotPresent` rewrite above (patch_pod_spec)
        # only touches containers that already appear in a manifest's own pod spec —
        # the operator Deployment's container gets it for free. But node provisioning
        # (AddNode) creates a SEPARATE one-shot Job whose container spec is built in
        # Go (services/operator/internal/provision/provision.go), not YAML, so this
        # rewrite can never reach it. OPERATOR_PROVISIONER_IMAGE_PULL_POLICY is the one
        # lever that does: the operator reads it and sets the Job container's
        # ImagePullPolicy itself. The rig side-loads kanz-provisioner via `kind load`
        # and has no credentials to pull ghcr.io/kanz-eng/* (private), so without this
        # the provisioning Job defaults to Always on its :latest tag and sits in
        # ImagePullBackOff even though the image is already on the node.
        {"name": "OPERATOR_PROVISIONER_IMAGE_PULL_POLICY",
         "value": "IfNotPresent"},
    ],
}


def _drop_on_rig(name, keep_spiffe=False):
    if name in _DROP_ENV_EXACT:
        return True
    # Any SPIFFE workload-API socket (SPIFFE_ENDPOINT_SOCKET, *_SPIFFE_SOCKET, ...).
    if keep_spiffe:
        return False
    return "SPIFFE" in name and "SOCKET" in name


def patch_pod_spec(spec, path, keep_spiffe=False):
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
                container["env"] = [
                    e for e in env if not _drop_on_rig(e.get("name", ""), keep_spiffe)
                ]

        # Additions apply to the workload's own containers, never its init
        # containers: an initContainer runs migrations, not the service.
        for container in spec.get("containers") or []:
            for add in _ADD_ENV.get(os.path.basename(path), ()):
                have = {e.get("name") for e in container.get("env") or []}
                if add["name"] not in have:
                    container.setdefault("env", []).append(dict(add))


def main():
    # --keep-spiffe LEAVES the SPIFFE workload-API socket env in place.
    #
    # Dropping it is right only where the bus is the dev-PLAINTEXT deviation: with the
    # socket set, transport.NewMesh(...).Enabled() is true and the workload dials NATS
    # over mTLS, which a plaintext server refuses ("secure connection not available").
    #
    # But infra/nats/nats.yaml serves `tls { verify: true, verify_and_map: true }` —
    # mTLS ONLY. On a cluster that runs the in-repo SPIRE (rig-apply.sh --spire) and
    # that NATS, the drop inverts: the socket is REQUIRED, and stripping it leaves every
    # workload dialling plaintext at a server that will not answer. The SPIFFE CSI
    # volumes are already left alone for exactly this reason; this flag lets the env
    # agree with them.
    #
    # Default stays OFF so the existing plaintext rig is unchanged.
    args = [a for a in sys.argv[1:] if a != "--keep-spiffe"]
    keep_spiffe = "--keep-spiffe" in sys.argv[1:]
    if len(args) != 1:
        sys.exit("usage: rig_dev_patch.py [--keep-spiffe] <manifest.yaml>")

    with open(args[0]) as fh:
        docs = [d for d in yaml.safe_load_all(fh) if d]

    if not docs:
        sys.exit(f"FATAL: {args[0]} parsed to no documents")

    for doc in docs:
        # Deployment/StatefulSet/DaemonSet/Job and the Argo Rollout all carry the
        # pod spec at the same path.
        tmpl = (doc.get("spec") or {}).get("template") or {}
        if tmpl.get("spec"):
            patch_pod_spec(tmpl["spec"], args[0], keep_spiffe)

    yaml.safe_dump_all(docs, sys.stdout, default_flow_style=False)


if __name__ == "__main__":
    main()
