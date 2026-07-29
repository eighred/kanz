#!/usr/bin/env python3
"""Adapt a production manifest for the dev rig, on stdout.

Four rig-only transformations, each because the rig is deliberately unlike prod
(Vault CSI -> dev Secret; digest -> :latest tag; :latest -> IfNotPresent; SPIFFE
bus mesh -> plaintext, since the rig's NATS is dev-plaintext). See the inline
comments for each:

1. Vault CSI -> dev Secret. Production mounts DSNs and credentials from Vault via
   secrets-store.csi.k8s.io. The dev rig has no Vault (ONBOARD-M6), so each such
   volume becomes a reference to the committed rig-dev-secrets Secret.

2. ghcr.io/eighred/<service>[:tag]@sha256:<digest> -> ghcr.io/eighred/<service>:latest
   on every container. release.yml's pin-digests job (SUPPLY-M1/OPS-M4a) rewrites
   every production manifest's image to an immutable digest computed from that
   CI build. The rig never runs that build: tools/rig-images.sh builds each
   service from source and `kind load`s it into the node as
   ghcr.io/eighred/<service>:latest, and a locally built image can never
   reproduce a CI-computed digest — content-addressing means no local rebuild
   will ever equal it byte-for-byte. Applying the manifest unmodified asks the
   kubelet for a reference no image on the node can satisfy, so it falls back to
   the private ghcr.io registry the node holds no credential for: ImagePullBackOff
   on every rig pod. Rewriting to the same :latest tag rig-images.sh built makes
   the transformation below (IfNotPresent) actually able to find a match. Only
   references to OUR OWN registry are touched; third-party images (gcr.io/
   distroless/..., ghcr.io/spiffe/..., apache/kafka, ...) the rig pulls normally
   and are left byte-identical. A reference carrying a DIGEST is rewritten to
   :latest regardless of whether it also carries a tag: per OCI reference
   resolution, when a reference carries both a tag and a digest, the digest is
   authoritative for the pull — the tag is cosmetic — so a tag+digest reference
   is exactly as unpullable on the rig as a bare digest and must be rewritten
   the same way. Only a reference carrying a tag and NO digest passes through
   unchanged, since that is already what the rig wants. A ghcr.io/eighred/*
   reference in a shape this rewrite does not recognize is a FATAL error, not a
   silent pass-through: silently leaving a digest in place is exactly the
   confusing ImagePullBackOff this transformation exists to remove, so guessing
   wrong here is worse than refusing to guess.

3. imagePullPolicy -> IfNotPresent on every container. Kubernetes defaults
   imagePullPolicy to Always for a :latest tag — including the :latest tag
   transformation 2 above just produced — and Always means the kubelet ignores
   whatever `kind load` already put on the node and dials the private,
   unauthenticated ghcr.io registry instead: ImagePullBackOff. Forcing
   IfNotPresent makes the kind-loaded image authoritative. Production keeps
   Always (moot anyway once the manifest is digest-pinned, since a digest is
   immutable and Always vs. IfNotPresent make no observable difference) — this
   belongs in the rig patch, not the committed manifest. It also retires the
   hand-patch that did this on the rig on 2026-07-12 and recorded nowhere.

4. SPIFFE workload-API socket env, and a couple of production-only endpoint
   env vars, dropped so the workload falls back to its plaintext / in-process
   dev path instead of dialling infrastructure the rig does not run. See the
   detailed per-variable reasoning inline in patch_pod_spec below.

csi.spiffe.io volumes are deliberately LEFT ALONE: after `rig-apply.sh --spire`
the rig runs real SPIRE, so its SVIDs are genuine and stripping them would
reintroduce the very deviation this replaces.
"""
import os
import re
import sys
import yaml

VAULT_DRIVER = "secrets-store.csi.k8s.io"

# Our own registry. Only references under this prefix are ever rewritten;
# everything else (gcr.io/distroless/*, ghcr.io/spiffe/*, apache/kafka, ...) is
# a third-party image the rig pulls normally and must be left byte-identical.
OWN_REGISTRY_PREFIX = "ghcr.io/eighred/"

# Matches ghcr.io/eighred/<service>[:tag]@sha256:<digest> — what release.yml's
# pin-digests job (SUPPLY-M1/OPS-M4a) writes into every production manifest,
# WITH OR WITHOUT an accompanying tag. Per OCI reference resolution the digest
# is authoritative over any tag also present, so a tag+digest reference is
# handled identically to a bare-digest one — the optional `(?::tag)?` group
# below is what makes that so. Group 1 is the service name, reused to build
# the rig's :latest reference.
_OWN_DIGEST_RE = re.compile(
    r"^" + re.escape(OWN_REGISTRY_PREFIX) +
    r"([a-z0-9][a-z0-9._-]*)(?::[A-Za-z0-9._-]+)?@sha256:[a-f0-9]{64}$")

# Matches ghcr.io/eighred/<service>:<tag> — a tag reference carrying NO
# digest. This is the ONLY own-registry shape the rig passes through
# unchanged: with no digest present, the tag is what actually resolves the
# pull, and it is already what the rig wants (see _rig_image below). Any
# digest-bearing reference — tagged or not — is matched by _OWN_DIGEST_RE
# above instead, never here.
_OWN_TAGGED_RE = re.compile(
    r"^" + re.escape(OWN_REGISTRY_PREFIX) +
    r"[a-z0-9][a-z0-9._-]*:[A-Za-z0-9._-]+$")


def _rig_image(image, path, container_name):
    """Return the rig-appropriate image reference for `image`.

    Only ghcr.io/eighred/* references are touched — everything else (a
    third-party image) is returned unchanged. Any ghcr.io/eighred/* reference
    that carries a digest — whether bare (@sha256:<digest>) or alongside a
    tag (:<tag>@sha256:<digest>) — is rewritten to :latest, because per OCI
    reference resolution the digest is what the registry actually resolves;
    an accompanying tag does not make it any more pullable on the rig. Only a
    reference carrying a tag and NO digest is already what the rig wants and
    passes through unchanged. A ghcr.io/eighred/* reference in neither shape
    is one this function does not understand, and it fails loudly rather than
    silently leaving a possibly-unpullable reference in place — the same
    contract as the Vault CSI FATAL below.
    """
    if not image.startswith(OWN_REGISTRY_PREFIX):
        return image

    m = _OWN_DIGEST_RE.match(image)
    if m:
        service = m.group(1)
        # tools/rig-images.sh builds and `kind load`s exactly this reference;
        # rig_dev_posture_test.go's TestRigDevPatchRewritesDigestPinnedImagesToLocalTags
        # pins that the two agree.
        return OWN_REGISTRY_PREFIX + service + ":latest"

    if _OWN_TAGGED_RE.match(image):
        # A tag reference with NO digest — already what the rig wants, so pass
        # through unchanged. A digest-bearing reference, even one that also
        # carries a tag, is matched by _OWN_DIGEST_RE above instead and never
        # reaches here: the tag is cosmetic once a digest is present, and
        # leaving the digest in place would still be unpullable on the rig.
        # This is what lets --keep-spiffe-style local experiments (or a
        # manifest edited by hand to a tag) round-trip through the patcher
        # without this rewrite fighting them.
        return image

    sys.exit(f"FATAL: {path}: container {container_name!r} uses image {image!r}, "
              f"which carries our own registry prefix ({OWN_REGISTRY_PREFIX}) but is "
              f"in a reference shape this rewrite does not recognize (neither "
              f"@sha256:<digest> nor :<tag>). Silently leaving it as-is risks a "
              f"reference the rig's kind-loaded image cannot satisfy, which is the "
              f"exact ImagePullBackOff this rewrite exists to prevent — teach this "
              f"function the new shape rather than let it guess.")

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
        # PROSPECTIVE — THIS ENTRY IS NOT REACHED BY THE DOCUMENTED RIG FLOW TODAY.
        # tools/rig-apply.sh's workload list is oms, tv-sync, api-gateway,
        # webhook-ingest and compliance; it never applies operator-deploy.yaml, so
        # nothing below fires on a plain `rig-apply.sh` and a fresh rig re-provision
        # reintroduces the ImagePullBackOff described here.
        #
        # DO NOT "FIX" THIS BY ADDING operator-deploy.yaml TO THAT LIST. The manifest is
        # now pinned with nodeSelector node-role.kubernetes.io/control-plane: "true" (a
        # nodeSelector is an exact string match, and kind labels its control plane with
        # an EMPTY value, not "true"), so applying it to the rig unmodified strands the
        # operator Pending — a worse failure, and one this file has no rule for.
        #
        # The operator is HAND-APPLIED to the rig when it is needed there, and this rule
        # is what makes that hand-apply correct:
        #   python3 tools/rig_dev_patch.py kanz/infra/deploy/operator-deploy.yaml \
        #     | sed 's|control-plane: "true"|control-plane: ""|' \
        #     | kubectl apply -f -
        # Keep the entry: it is the only place the reason below is written down, and the
        # cost of carrying it is zero until either rig-apply.sh grows a node-label rewrite
        # or the pin learns to tolerate both label values.
        #
        # The generic `imagePullPolicy: IfNotPresent` rewrite above (patch_pod_spec)
        # only touches containers that already appear in a manifest's own pod spec —
        # the operator Deployment's container gets it for free. But node provisioning
        # (AddNode) creates a SEPARATE one-shot Job whose container spec is built in
        # Go (services/operator/internal/provision/provision.go), not YAML, so this
        # rewrite can never reach it. OPERATOR_PROVISIONER_IMAGE_PULL_POLICY is the one
        # lever that does: the operator reads it and sets the Job container's
        # ImagePullPolicy itself. The rig side-loads kanz-provisioner via `kind load`
        # and has no credentials to pull ghcr.io/eighred/* (private), so without this
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
            # Production is digest-pinned (release.yml's pin-digests job); the rig
            # cannot reproduce that digest from a local build, so rewrite our own
            # images to the :latest tag tools/rig-images.sh actually built and
            # `kind load`ed. See _rig_image and this file's module docstring
            # (transformation 2) for the full reasoning. Third-party images are
            # returned unchanged.
            image = container.get("image")
            if image:
                container["image"] = _rig_image(image, path, container.get("name"))

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
