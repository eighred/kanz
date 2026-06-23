# Runtime hardening (SEC-02a)

Extends the build-side supply-chain gate (CICD-01d signing, SEC-02b admission)
to the **running pod**: shrink what a workload can do and who it can talk to, so
a compromise is contained.

| File | What it does |
|---|---|
| `pod-security.yaml` | Pins `kanz-services` to the **restricted** Pod Security Standard (cluster-enforced non-root / no-priv-esc / dropped-caps / seccomp) + the canonical hardened `securityContext` as policy-as-data |
| `network-policies.yaml` | **Default-deny** ingress+egress in `kanz-services`, then the minimum explicit allows (DNS, NATS/Kafka, OTLP, gateway→risk-engine, ingress→gateway, scrape) |

The per-workload `securityContext` is applied on the actual deployments
(`infra/deploy/risk-engine-rollout.yaml`): `runAsNonRoot`, `runAsUser: 65532`
(distroless `:nonroot`), `readOnlyRootFilesystem: true` (writes → an explicit
`/tmp` emptyDir), `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`,
`seccompProfile: RuntimeDefault`.

## Two layers, on purpose

- **securityContext** (the deployment) is the *intent* — each pod declares its
  hardened posture.
- **Pod Security Standards** (the namespace label) is the *enforcement* — the
  built-in admission gate rejects any pod that omits the posture, so a new or
  drifted workload can't ship soft. Pinned to `enforce-version: v1.31` so a
  cluster upgrade can't silently move the bar.

The images are already distroless + static + non-root (the service Dockerfiles,
CICD-01c); this makes the *kubelet* enforce what the image assumes.

## Network model

`default-deny-all` selects every pod and allows nothing; each subsequent policy
opens one flow. NetworkPolicy is additive and deny-by-default once a pod is
selected, so adding a service means adding its allows — never relaxing the
default. Requires a NetworkPolicy-enforcing CNI (Cilium/Calico), which the
INFRA-01a cluster runs. This is the network complement to SEC-01's identity
zero-trust: mTLS proves *who*, NetworkPolicy bounds *reachability*.

## gVisor

`seccompProfile: RuntimeDefault` is the syscall allowlist. For the most
sensitive workloads, a gVisor `RuntimeClass` (`runtimeClassName: gvisor`) adds a
userspace kernel boundary — a per-workload override layered on this baseline, not
a default (the syscall-interception overhead isn't free).

## Apply

```sh
kubectl apply -f pod-security.yaml      # namespace labels + reference context
kubectl apply -f network-policies.yaml  # default-deny + allows
```

Verified by `infra/security/test/` (SEC-02e) and the SRE-01c chaos suite (a pod
that can't reach the spine should fail closed, not silently degrade).
