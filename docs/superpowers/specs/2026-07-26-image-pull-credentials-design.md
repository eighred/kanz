# OPS-M2f-b — Image pull credentials for the private registry

**Date:** 2026-07-26
**Board row:** OPS-M2f-b (`KANZ_TASKS.md` → TODO → Buildable now)
**Status:** design approved; live proof pending a reachable cluster

## The problem, as verified

Every platform image is private and nothing in the estate can authenticate to pull it.

| fact | evidence |
|---|---|
| 39 `ghcr.io/kanz-eng/*` references across `infra/`, 25 distinct services | `grep -rho "image: ghcr\.io/kanz-eng/[a-z0-9-]*" kanz/infra/` |
| every one carries `:latest` | same grep with `:latest` returns the same 39 |
| zero `imagePullSecrets` in any manifest | `grep -rn imagePullSecrets` hits only two *comments*, in `provision.go` and `operator_placement_test.go` |
| zero `registries.yaml`, zero `dockerconfigjson` anywhere | a `grep -rn` alternation over `registries.yaml`, `dockerconfigjson` and `docker-registry` returns nothing |
| the provisioner join has no registry step | `cmd/kanz-provisioner/join.go:38` — `k3sInstallCmd` is a single piped `curl`-into-`sh -s - agent` line |
| `ghcr.io/kanz-eng/*` is private | 403 to anonymous pulls |

A node joined through the TUI therefore holds none of the platform images, and any
workload scheduled onto it fails `ErrImagePull`. This was observed twice during
OPS-M2e — once for a probe Job, once for the operator Deployment's own rollout —
and worked around by hand (`ctr export` → `scp` → `ctr import` across ten images).
Until it is fixed, **adding node capacity makes the estate less reliable, not more**,
which inverts the purpose of the Add Node workflow.

## Decisions

### Credential: machine-account fine-grained PAT

A dedicated bot GitHub account, fine-grained PAT scoped to `read:packages` on the
`kanz-eng` org only. Chosen over a GitHub App installation token (which buys
short-lived credentials at the cost of a new in-cluster refresher component, and
relocates rather than removes the secret you must protect) and over a personal PAT
(which makes the estate's ability to pull images depend on one person's account —
a revoked or expired personal token is a fleet-wide `ErrImagePull` with no obvious
cause).

### Mechanism: `imagePullSecrets` on ServiceAccounts

A `kubernetes.io/dockerconfigjson` Secret named `ghcr-pull` in each namespace that
runs private images, attached via `imagePullSecrets` on the ServiceAccounts that own
those pods.

**Rejected: node-level `/etc/rancher/k3s/registries.yaml` written at join.** It is the
k3s-idiomatic answer and needs zero manifest edits, but it puts a fleet-wide registry
credential on every node's disk and makes rotation an SSH sweep of the fleet — the exact
outcome the OPS epic exists to eliminate (`KANZ_BRAIN.md` → "Operational control plane":
SSH and `kubectl` are disaster-recovery tools). It also only covers nodes joined by the
provisioner: a node joined by OPS-M3 local bootstrap, or by hand, silently lacks the file
and fails later as an `ErrImagePull` nobody connects back to this decision.

**Rejected: make the registry public.** It removes the problem rather than managing it,
and the integrity property survives untouched — `infra/security/admission/cluster-image-policy.yaml`
still admits only `release.yml`-signed images. The cost is that a proprietary trading
system's container images become world-readable.

**Why the ServiceAccount and not the pod spec.** The credential attaches at 26 points
instead of 39, each one line beside the workload it serves, in the file that already
declares that ServiceAccount. More importantly the two operator-built Job specs
(`services/operator/internal/provision/provision.go:247` and `:508`) already set
`ServiceAccountName: "kanz-node-provisioner"`, so they inherit the credential with **no Go
change at all**. Attaching at the pod spec would have required editing Go-constructed pod
specs and would have left a second enumeration to drift.

## Scope

Three namespaces run private images; the rest of the `ghcr.io/kanz-eng` hits are not
workloads.

| namespace | ServiceAccounts | note |
|---|---|---|
| `kanz-services` | 22 | one per service, each declared in its own `*-deploy.yaml` / `*-rollout.yaml` |
| `kanz-operator` | 3 | `operator`, `kanz-halt`, `kanz-node-provisioner` |
| `kanz-messaging` | 1 | `nats-rebuild` |

Not workloads, and therefore exempt from the guard with a stated reason each:
`infra/security/admission/cluster-image-policy.yaml` (a glob inside a policy CR),
`infra/gitops/preview-applicationset.yaml` (the CR itself lives in `argocd`, but it renders
`infra/deploy` — private-image Deployments included — into on-demand `kanz-preview-{number}`
namespaces that the three-namespace bootstrap in this document never covers; preview
environments were already broken this way before this branch, and this branch does not
make it worse), `infra/security/admission/README.md`, `infra/security/test/README.md`,
`infra/security/test/verify-admission.sh`.

## Lifecycle

**Bootstrap is out-of-band, once, and that is not a workaround.** The first Secret cannot
come from inside the cluster: the operator that would write it runs a private image itself.
So bootstrap is one documented `kubectl create secret docker-registry ghcr-pull …` per
namespace, recorded in `DEMO_DEPLOYMENT.md` as a **bootstrap** step — distinct from the
daily path, which is what that document exists to separate.

**Rotation is three Secret updates and no SSH.** Every node picks the new credential up on
its next pull; nothing on any node's disk changes.

**Rotation from the TUI is explicitly out of scope for this task.** The board's
verified-when does not ask for it, mechanism A's rotation story holds without it, and an
RPC plus a form would push this past three engineer-days. It belongs on the board as its
own row, shaped like the existing write-only `SetVenueKeys` seam so the credential exists
as bytes only in the request and the Secret — never a model field, never rendered, never
logged.

## Why this is a named exception to the Vault pattern

`KANZ_BRAIN.md` → "Security & multi-tenancy" states that secrets come from Vault via CSI
mounts, KMS-rooted. That cannot deliver this one. The Vault CSI driver materializes a
Kubernetes Secret only when a pod mounting the volume is running, and no pod runs until
its image has been pulled. **The pull credential is the one secret that must exist before
the mechanism that delivers every other secret.** This is recorded in `KANZ_BRAIN.md`
because the next engineer will otherwise try to move it into Vault and rediscover the
deadlock from the inside.

## A comment that becomes false when this lands

`services/operator/internal/provision/provision.go:65–71` gives two justifications for
pinning the provisioning and probe Jobs to the control plane. The second is:

> The estate holds no registry credentials (ghcr is private; anonymous pulls 403) and
> nothing in `infra/` carries `imagePullSecrets`, so a Job scheduled onto a node that has
> not pre-loaded the kanz-provisioner image dies in `ErrImagePull`.

This change deletes that reason. Only credential confinement remains. The comment is
corrected in the same commit — a comment defending a security trade-off on a premise that
is no longer true is dated evidence masquerading as current truth, and this repository has
already paid for that once. It also sharpens OPS-M2f-a, which now has exactly one
justification to replace rather than two.

## Interaction with the dev rig

`tools/rig_dev_patch.py` rewrites `imagePullPolicy` to `IfNotPresent` for the rig, because
the rig runs images `kind load`ed from local builds and has no registry access. That seam
stays and this change does not remove its reason. The patch must not strip the new
`imagePullSecrets` — verified by inspection, not by a test: `main()` only descends into
`doc["spec"]["template"]["spec"]`, and a ServiceAccount document has no `spec` key, so the
top-level `imagePullSecrets` this change adds is never reached by the rewrite. No test
covers this; if `rig_dev_patch.py`'s traversal changes, re-check by reading it again.

## Testing

**`TestPrivateImagesHavePullSecrets`** — `test/arch/supplychain_test.go`, following that
file's established shape (`moduleRoot(t)`, a non-vacuity `t.Fatal`, an exemption list where
each entry carries a reason). Walk `infra/`; for every manifest referencing
`ghcr.io/kanz-eng/`, resolve the pod's `serviceAccountName`, locate that ServiceAccount's
definition, and assert it carries `imagePullSecrets: [{name: ghcr-pull}]`. Fatal if zero
manifests matched, so the test cannot pass by matching nothing.

**Non-vacuity proof.** The guard is demonstrated to fail: remove `imagePullSecrets` from
one ServiceAccount, observe the named failure, restore. A guard never seen red is a guard
nobody has proven works.

**`automountServiceAccountToken: false` is not a conflict.** `TestProvisionerServiceAccountHasNoToken`
asserts that flag on `kanz-node-provisioner`. It governs the projected API token only;
kubelet reads `imagePullSecrets` off the ServiceAccount regardless. Stated here because it
reads like a contradiction and is not.

## Verification

_Verified when_ — from the board, unchanged:

1. A node provisioned through the TUI runs a platform workload with **no manual image
   step**, proven by scheduling one onto it immediately after join.
2. An arch guard asserts the chosen mechanism is present, so its removal fails the build.

(2) is achievable in this session. **(1) requires a live cluster and none is reachable from
this box** (`127.0.0.1:56566` refused). It ships as **pending, not claimed**. The branch
this work sits on exists because an unguarded claim survived to the readiness board; the
same standard applies to its own successor.

## Out of scope, recorded

**The sigstore policy-controller is a second consumer of registry credentials.**
`infra/security/admission/cluster-image-policy.yaml` is a fail-closed `ClusterImagePolicy`
over `ghcr.io/kanz-eng/**`, so the webhook must itself read signatures and attestations
from the private registry. It is **not** wired here, because policy-controller has no
install path anywhere in this repository — only the CR and a README pointing upstream — so
the policy is currently inert and wiring its credential now would be unverifiable work.
This becomes live the day policy-controller is installed, and is recorded so that day does
not begin with a mystery.
