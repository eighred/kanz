# Tokyo capital-path workload overlay

This overlay is the immutable image lock for the cost-bounded single-node
testnet. It reuses the canonical workload manifests, replaces only their image
references with same-account Tokyo ECR digests, removes their GHCR pull-secret
dependency, and scales workloads to one replica. It does not weaken application
risk, compliance, mandate, mTLS, tenant, or secret-provider boundaries.

Use the repository-root installer from an exact, clean `origin/main` checkout.
It renders the reviewed overlay locally, retains only the canonical Vault
classes mounted by this workload graph, rejects mutable or non-Tokyo images,
proves that every digest is still retained under the annotated ECR release,
transfers a hashed manifest over SSM, verifies the data-plane prerequisites, and
runs a server-side dry-run on the node. Dry-run is the default:

```powershell
.\tools\Install-TestnetWorkloads.ps1 -InstanceId <managed-node-id>
```

Apply the same checked manifest only after reviewing the dry-run evidence:

```powershell
.\tools\Install-TestnetWorkloads.ps1 -InstanceId <managed-node-id> -Apply
```

The apply path waits for all eight Deployments and the risk-engine Rollout, then
requires nine ready Pods whose running SHA-256 image IDs match the reviewed ECR
lock. It also proves that no registry credential Secret exists in
`kanz-services`. A failed first installation deletes the exact rendered object
set; an ambiguous partial installation is refused before apply. The installer
submits no request or order to any service.

Do not apply until the namespace, SPIFFE registrations, Vault
`SecretProviderClass` objects, NATS, Postgres migrations, and Argo Rollouts CRD
are healthy. The digest lock identifies build commit
`a8e3d7c1f42302b79485488c435da7386040a2b5`; changing it requires resolving all
ten workload/migration images from one successful main build and rerunning the
overlay architecture guard.

Before the first workload install, use option 3 in
`tools/Kanz-Venue-Secrets.cmd`. The data-plane bootstrap preserves complete
Postgres and Redis credential groups, refuses partial groups, and creates the
gateway signing secret and Identity P-256 key inside Vault when absent. It does
not rotate an existing signing key on a rerun.

The `kanz-capitalpath` certifier image is intentionally not a standing workload.
Pass its ECR digest separately to `infra/dr/run-capitalpath-certification.sh`
only after the DR attestation and order safety gates are green.
