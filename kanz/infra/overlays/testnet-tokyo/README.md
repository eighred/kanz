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

The apply path waits for all nine Deployments and the risk-engine Rollout, then
requires ten ready Pods whose running SHA-256 image IDs match the reviewed ECR
lock. It also proves that no registry credential Secret exists in
`kanz-services`. A failed first installation deletes the exact rendered object
set; an ambiguous partial installation is refused before apply. The installer
submits no request or order to any service.

Tokyo runs one risk-engine replica. Its overlay replaces the canonical 20/50%
replica-weighted steps, which would round to zero canary Pods at that scale, with
one 100% canary behind `maxSurge: 1` and `maxUnavailable: 0`. The stable Pod stays
available until the canary is Ready; the same Prometheus analysis then admits or
aborts that real canary before promotion.

Do not apply until the namespace, SPIFFE registrations, Vault
`SecretProviderClass` objects, NATS, Postgres migrations, and Argo Rollouts CRD
are healthy. The default digest lock identifies build commit
`8e28e37d1780c589fa90b512863394f8ac12a8ce`. Identity/gateway and the browser edge
have explicit per-workload release overrides. Each workload's containers must
resolve to the successful main build named by its release annotation; rerun the
overlay architecture guard and exact-main installer dry-run after changing pins.

The browser-security release is `33858a909df304ced9779344c87ed1e25a8cb237`.
Apply its rendered `web-bff` Deployment only after the image canary and reviewed
server dry-run. Recreate prevents overlapping in-memory session owners, so expect
a brief web interruption and sign in again. Verify the live edge as described in
`../../edge/README.md`. This release does not change trading state.

For the initial transition from RollingUpdate, apply the rendered `web-bff`
Deployment as a JSON merge patch with `--field-manager=kanz-bootstrap`, after
running that same patch with `--dry-run=server`. The explicit
`strategy.rollingUpdate: null` removes retained API defaults atomically with the
image/template update; a server-side apply alone can retain those defaults and
reject Recreate. Preserve the pre-update controller for rollback, require its
resourceVersion as a precondition, wait for both containers to be Ready at the
reviewed digests, and rerun the whole-overlay installer dry-run afterward.

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


### Disposable CNI capability proof

`tools/Test-TestnetCNI.ps1` is a separate, explicitly invoked operator check.
It refuses a checkout that is not exact, unmodified `origin/main`, an unexpected
AWS account/region/instance, or a different `kube-system` UID. It creates only a
random `kanz-cni-proof-<uuid>` namespace with three bounded, unprivileged synthetic
pods; it does not contact brokers, venue adapters or domain services. No caller
can select a target or fixture namespace. Review and merge its exact-source CI
proof before invoking it through the normal authenticated operator profile.

The same Python engine runs in disposable kind with the shared pinned Cilium
installer. A CI-only negative control disables enforcement in that disposable
cluster, verifies the loaded Cilium mode, and requires baseline success followed
by default-deny failure and verified cleanup. The operator has no such override.
The engine compares Pod-IP and Service-IP baseline, default-deny, selected
allow/unselected deny, and post-removal restoration: 16 observations plus verified
namespace cleanup with UID-preconditioned deletion. Every pod must match the
pinned image and loaded manifest digest; actual image IDs are retained. Socket
timeout/refusal counts only within those positive
controls; command errors and unknown results fail. Cleanup uncertainty prevents
PASS. The operator emits sanitized source/image/namespace/case/cleanup evidence.

This proves only the fixture's NetworkPolicy behavior on the measured cluster.
It does not certify production allow-sets, namespace isolation coverage,
TradingView/tv-sync activation or permission to resume capital execution. Keep
#232 and the separate #1410 promotion acceptance open. Cluster reinstallation
requires a reviewed UID update rather than an override.

Fixture image: official Python 3.13.18 Alpine linux/amd64 manifest, digest pinned in
`tools/cni-proof/proof.py`; [Python CLI](https://docs.python.org/3.13/using/cmdline.html)
and [socket outcomes](https://docs.python.org/3.13/library/socket.html) define the
probe behavior. No third-party Python packages run inside the fixture pods.

## Preparing tv-sync without activating it

`tv-sync-preparation/` is an explicit operator overlay. It is not referenced by
this workload overlay or the ordinary `data/` overlay. Ordinary data installation
retains its original 37 resources, eight databases and sixteen roles; ordinary
Vault bootstrap does not inspect or create tv-sync credentials. The shared
provisioner script accepts only its default mode and the fixed `tv-sync` mode.
The latter runs in a separate Job with the existing migration ServiceAccount and
a dedicated two-password CSI class, using the same SQL grant implementation.

After these source prerequisites are merged, establish the separate registry
prerequisite and successful signed promotion/custody for the exact v2026.4.0 pair.
`tv-sync-release.json` is a byte-pinned desired tuple, not evidence of promotion.
Its explicit `release-v2026.4.0` tag avoids overwriting the existing immutable
full-SHA migration tag, which identifies a different artifact. Ordinary image
resolution retains the full-SHA convention. The operator rechecks both paired
destination tags and digests; retain the independently verified signature,
attestation and descriptor-graph evidence from promotion as an activation gate.

Use the existing private Vault launcher with `-Target DataPlane -Mode Bootstrap
-PrepareTVSync`. It verifies merged source, Tokyo account and instance before
staging, requires the existing Vault baseline and qualifies only an all-or-none
three-field group: provisioner app and
migration passwords plus the runtime-only `kv/kanz/tv-sync-runtime` DSN. The
runtime role receives only that separate record after qualification. Forbidden
`migrate_dsn`, partial records and unknown lookup errors refuse. This mode neither generates
unrelated credentials nor rewrites legacy policies/auth config; it records a
separate tv-sync completion marker. Expire/revoke
previously issued runtime tokens before eventual activation; other services'
legacy shared DSN layouts are not changed by this preparation.

`Install-TestnetDataPlane.ps1 -InstanceId i-0ee1235614219f089 -PrepareTVSync`
performs a dry run against the reviewed cluster UID. Add `-Apply` only for the
separately authorized preparation operation. It requires existing database and
base Jobs to be ready/complete with no running owned Pods before changing the
shared script ConfigMap. It creates only the tv-only CSI class and two fixed
Jobs, runs provisioning before migration, and verifies current Job UID, owner
reference, exact image and loaded digest. Existing preparation Jobs must already
be complete with matching immutable specifications and shared script; changed or
unfinished Jobs refuse rather than being deleted/recreated. A rerun with the same
qualified state reuses them.

On failure, cleanup can stop only newly created Jobs carrying this invocation's
ownership annotation and UID; preexisting Jobs are never deleted. It restores
the captured ConfigMap with resource-version checks and removes its newly created
CSI class. Foreign replacements or concurrent changes leave an explicit
unverified rollback result. This restores Kubernetes configuration, not SQL:
never drop provisioned databases, roles, fact history or checkpoints as cleanup.
Synthetic operator tests prove these branches; they do not prove live rollback.

Preparation does not deploy tv-sync, change gateway routes/NetworkPolicy, or
expose API 8113. Runtime CSI remains unreferenced. Actual Vault/CSI/SPIRE/NATS,
role/future-grant/RLS, history/checkpoint backup/restore, policy-union/CNI and
controlled outage/rollback acceptance still precede the separate activation in
#1440/#1410. No capital permission follows from a successful preparation Job.
