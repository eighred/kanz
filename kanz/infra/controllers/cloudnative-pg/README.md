# CloudNativePG controller for Tokyo testnet

CloudNativePG 1.30.0 is pinned because it supports Kubernetes 1.36 and contains
the 2026 role-management and operator security fixes. The upstream release
manifest is downloaded at execution time, checked against `release-lock.json`,
then rendered through this overlay so the controller image is immutable and the
single-node resource envelope is explicit.

The controller egress policy pins the live Tokyo Kubernetes API endpoint
(`10.71.0.15:6443`). kube-router evaluates policy after Service DNAT, so allowing
the `kubernetes.default` ClusterIP does not admit the connection. If the node is
replaced, update this /32 from `kubectl -n default get endpoints kubernetes`
before reinstalling; a stale value fails closed.

Run a server-side dry run first:

```powershell
.\tools\Install-TestnetCloudNativePG.ps1 -InstanceId i-0ee1235614219f089
```

Pass `-Apply` only after the rendered data-plane manifest and Vault bindings
have passed their preflight checks.
