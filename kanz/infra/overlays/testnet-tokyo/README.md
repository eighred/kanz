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

Every OMS starts halted. Before any operator publishes a resume FACT, run the
merged, repository-owned capital-admission preflight:

```powershell
.\tools\Test-TestnetOmsGoLive.ps1 -InstanceId <managed-node-id>
```

The preflight is read-only and refuses an OMS unless mandate enforcement,
unique portfolio-to-account binding, exchange account proof, and dual control
with an explicit threshold are armed. It also requires one Ready OMS Pod and
rejects recent shared-collateral, unverified-account, advisory-mandate, absent
dual-control, or incomplete-margin warnings. A passing preflight is necessary
evidence for resume; it does not publish the resume FACT or submit an order.

The governance inputs have separate authoritative sources:

- Portfolio inventory starts with the typed `test/load/seed` event used by
  `infra/onboarding/provision-tenant.sh` and is materialized by risk-engine.
  An empty risk inventory is not complete coverage.
- Mandates are proposed and approved by two authenticated holders of
  `kanz-mandate-signatory` through the gateway's compliance listener, then
  retained as compacted FACTs in the MANDATE stream.
- `OMS_VENUE_ACCOUNTS` is the reviewed account-master mapping from
  `tenant/portfolio@MIC` to the adapter's logical account label. The Tokyo OMS
  refuses every unbound portfolio; the overlay does not invent a mapping.
- The Binance and OKX expected account UIDs come from independently reviewed
  exchange account-opening evidence. They must never be copied from the
  adapter's own signed observation. Until those values are configured and both
  adapter escape hatches are removed, verified-account enforcement remains
  unarmed and this preflight fails.
- Identities are created through Identity's invite/redemption flow. A trader
  and `kanz-order-approver` must be distinct active subjects in the same tenant;
  mandate approval likewise requires two distinct active signatories. The
  preflight reads only aggregate pair counts and never emits subject names or
  credential hashes.

Tokyo arms mandate, venue-binding, and all-orders dual-control enforcement even
while their inventories are empty. Those missing inputs produce explicit
refusals and trading remains startup-halted. Account-proof enforcement is the
exception because arming it before expected UIDs exist prevents the OMS from
becoming Ready and removes the observation surface needed to finish the proof.
