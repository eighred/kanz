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

To expose the same bounded result on the authenticated kanz-web **Preflight**
page, add `-PublishWebEvidence`. The switch writes only the non-secret verdict,
control observations, timestamp, SSM command ID, exact verifier commit, deployed
release commit, and four digest-pinned capital-path image references to the
`kanz-oms-preflight-evidence` ConfigMap. The page treats evidence older than 15
minutes, missing evidence, an incomplete control set, or an unreadable artifact
as `UNKNOWN`. Publishing evidence has no OMS, FACT, or order side effect.

The governance inputs have separate authoritative sources:

- Portfolio inventory starts with the typed `test/load/seed` event used by
  `infra/onboarding/provision-tenant.sh` and is materialized by risk-engine.
  An empty risk inventory is not complete coverage.
- Mandates are proposed and approved by two authenticated holders of
  `kanz-mandate-signatory` through the gateway's compliance listener, then
  retained as compacted FACTs in the MANDATE stream.
- `OMS_VENUE_ACCOUNTS` is the reviewed account-master mapping from
  `tenant/portfolio@MIC` to the adapter's logical account label. The Tokyo OMS
  refuses every unbound portfolio. Tokyo carries one reviewed non-secret map:
  `__system__/PF1` uses `binance-main` at `XBIN` and `okx-sub-1` at `XOKX`.
  The minimum topology has no market-data database, so it leaves
  `RISK_ENGINE_VENUE_ACCOUNTS` empty and does not register liquidation proximity;
  a mandate for this topology must not claim that measure is available.
- The Binance and OKX expected account UIDs come from independently reviewed
  exchange account-opening evidence. They must never be copied from the
  adapter's own signed observation. Option 4 in `tools/Kanz-Venue-Secrets.cmd`
  accepts them only inside the remote Vault process and stores them under
  venue-separated account-master paths. The adapters mount those values as
  files, reject an unreadable mount, and refuse a mismatch. Tokyo removes both
  unverified-account escape hatches and requires verified accounts in the OMS.
- Identities are created through Identity's invite/redemption flow. A trader
  and `kanz-order-approver` must be distinct active subjects in the same tenant;
  mandate approval likewise requires two distinct active signatories. The
  preflight reads only aggregate pair counts and never emits subject names or
  credential hashes.

## Initial mandate review

`mandate.json.example` is a deliberately incomplete proposal for the singleton
test portfolio. It uses an instrument allow-list because that constraint is
resolved directly from the position and does not assert the unavailable market
data, NAV, buying-power, or liquidation-proximity inputs. An authorized capital
owner must copy it to a working file outside the repository and replace every
`REPLACE_WITH_...` value with an approved mandate ID, canonical test instrument,
and effective time. Do not commit the completed decision merely to move it
between signatories.

Validate the authored protojson with the same domain validator used by both
mandate publishers. This command has no actor flag, opens no network connection,
and creates no decision artifact:

```powershell
go run ./cmd/kanz-mandate validate --tenant __system__ --file <reviewed-mandate.json>
```

Validation is not approval. The authorized proposer signs in through the
gateway and submits the exact validated value plus the capital-owner rationale
to `POST /v1/portfolios/PF1/mandate`. A different authenticated holder of
`kanz-mandate-signatory` reads
`GET /v1/mandates/pending-changes/{proposal_id}`, compares the full mandate,
reason, and digest to the reviewed decision, and explicitly approves or rejects
it at `POST /v1/portfolios/PF1/mandate/approve`. The gateway refuses
self-approval after canonicalizing the subject. Do not use the unauthenticated
break-glass CLI for this initial mandate.

After publication, rerun `Test-TestnetOmsGoLive.ps1`. A PASS remains evidence
for reassessment; it does not authorize a resume FACT or an order.

Tokyo arms mandate, venue-binding, and all-orders dual-control enforcement even
while their inventories are empty. Those missing inputs produce explicit
refusals and trading remains startup-halted. Account-proof enforcement is the
exception because arming it before expected UIDs exist prevents the OMS from
becoming Ready and removes the observation surface needed to finish the proof.
