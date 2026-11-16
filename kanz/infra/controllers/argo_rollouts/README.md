# Argo Rollouts control plane

This directory defines the controller required by the risk-engine Rollout. The
upstream release is deliberately not vendored: its generated CRDs are about
3 MB. `release-lock.json` instead pins the release asset's SHA-256, source
commit, and multi-architecture controller image digest. The installer downloads
the asset into an isolated directory, verifies the bytes before rendering, and
applies the local namespace, availability, and resource-bound patches.

The ordinary path is a server-side dry run:

```powershell
.\tools\Install-TestnetArgoRollouts.ps1 `
  -InstanceId i-0ee1235614219f089
```

After reviewing that evidence, installation is explicit:

```powershell
.\tools\Install-TestnetArgoRollouts.ps1 `
  -InstanceId i-0ee1235614219f089 `
  -Apply
```

The tool fails before rendering if GitHub serves bytes different from the lock.
It uses server-side apply without forced conflicts, waits for every Rollouts CRD
to become Established, waits for the controller Deployment to become Available,
and verifies that the Rollout API is discoverable. It never installs the kubectl
plugin and never creates or promotes an application Rollout.

`upstream-install.yaml` is a runtime-only input and must never be committed.
Changing the version requires reviewing the upstream diff and RBAC, updating all
four lock fields, resolving the new image digest, rerunning the architecture
guard, and proving a controlled canary abort/rollback before capital-path use.
