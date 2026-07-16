# MT-02 — per-tenant compute (Model A), OMS first

## Context

**Tenant provisioning creates storage, streams and policy — and no compute.** An onboarded tenant
gets a database role, NATS account and Kafka topics, and then has **no process to serve it**.

**Lead decision (2026-07-17): Model A — pod-per-tenant.** The code already votes for it and has
all along: `services/oms/cmd/oms/main.go:349` builds
`pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)` **once, at startup**, so the tenant is a
**process constant**, not a per-request value. Model B (shared runtime, request-scoped tenant)
would be a rewrite of the data layer. The system is not "between two models" — it is Model A with
provisioning that stops one step short.

**Scope: the OMS only.** Prove the pattern on the smallest surface, then extend. `archiver` and
`market-ingest` stay platform-level on `__system__` (archiver's own manifest: *"`__system__`
carries the platform's cross-cutting FACTs"*).

## Ground truth established by the controller — do NOT re-derive, and do NOT contradict

1. **`infra/deploy/` is RAW-SYNCED by ArgoCD** (`infra/gitops/applicationset.yaml`: `component:
   workloads, path: kanz/infra/deploy`, `directory: { recurse: true }`) with
   **`syncPolicy.automated: { prune: true, selfHeal: true }`**.
2. **THEREFORE: `kubectl apply` FOR PER-TENANT PODS IS FORBIDDEN.** ArgoCD would **prune** anything
   applied out-of-band that is not in git. A tenant's OMS would come up and be silently deleted
   minutes later. `KANZ_BRAIN.md` already states the rule: *"node deployment ceases to be an
   operator-plane RPC at all — it is a GitOps commit, authenticated by the git/CI/cosign chain, not
   by a human's key."* **Provisioning EMITS manifests; git deploys them.**
3. **`recurse: true` means the tenants directory MUST NOT live under `infra/deploy/`** — a
   `kustomization.yaml` there would be swept into the `workloads` Application as a raw manifest.
   Put it at **`kanz/infra/tenants/<tenant>/`**.
4. **No per-tenant DB credential is needed.** `internal/pg/pool.go:53-60` sets the tenant via
   `AfterConnect` → `set_config('app.tenant_id', …)`, and the RLS policy keys **only** on that GUC
   (`USING (tenant_id = current_setting('app.tenant_id', true))`); `current_user` appears in no
   policy. A per-tenant pod uses the **same service DSN** and differs only by `OMS_TENANT`.
5. **SPIFFE identity is declarative and derived from the ServiceAccount** —
   `spiffe://kanz.internal/ns/{namespace}/sa/{serviceAccountName}`. A per-tenant SA `oms-<tenant>`
   yields `spiffe://kanz.internal/ns/kanz-services/sa/oms-<tenant>`, which **must** be admitted in
   `infra/nats/tenancy.yaml` or the pod cannot reach the broker (this is SEC-M3, exactly).
6. **`tenancy.yaml` is EXCLUDED from raw sync** (`exclude: "{tenancy.yaml,…}"`) — it is applied by
   `tenantctl`. So the NATS user goes through tenantctl's path, not through git-sync.
7. **CI has NO kubectl and NO kustomize.** `kubectl kustomize` works on this machine but **not on
   the runner**. Any guard that shells out to kubectl **would skip in CI** — which is precisely the
   defect that let DATA-M6 sit red for two days. **The guard MUST be pure Go.**

## Global Constraints

- **Do not run `kubectl apply` anywhere in the provisioning path.** See ground truth #2.
- **Do not copy `oms-deploy.yaml`.** A copy drifts, and drift-with-nothing-comparing-it is this
  repo's signature defect. The overlay must reference the real manifest as a kustomize `resources`
  entry so the base is resolved at sync time and a new env var in the base is inherited for free.
- **Do not touch the dead per-tenant DB role** (`tenantctl.sh:403-406`, `kanz_tenant_${TENANT}`).
  It is a real defect — unused, isolating nothing, granting only risk-engine's 3 tables, and
  minting a password-bearing login credential stored nowhere — but it is **out of scope** and
  filed separately. Do not delete it and do not depend on it.
- `gofmt -l` clean, `go vet` clean, `go test ./... -count=1` green from `kanz/` (`GOFLAGS=-mod=mod`).

## Task 1 — the per-tenant overlay

**Files:** `kanz/infra/tenants/README.md` (new), `kanz/infra/tenants/_example/` (new).

1. A kustomize overlay at `kanz/infra/tenants/<tenant>/kustomization.yaml` that:
   - references the real base: `resources: [../../deploy/oms-deploy.yaml]` (**not a copy**),
   - `nameSuffix: -<tenant>` so Deployment/Service/SA become `oms-<tenant>`,
   - patches `OMS_TENANT=<tenant>` and `serviceAccountName: oms-<tenant>`,
   - adds the `ServiceAccount oms-<tenant>` (the base's SA is `oms`; `nameSuffix` renames the
     reference, but the SA object itself must exist — verify what the base actually declares and
     make the rendered output self-consistent).
2. Commit a **worked example** for a fictional tenant under `_example/` so the README is
   executable rather than aspirational, and so Task 3's guard has something to test against.
   `_example` must be named so it can never be mistaken for a real tenant.
3. **Verify by rendering**: `kubectl kustomize kanz/infra/tenants/_example/` (available on this
   machine) and paste the output. It must show `oms-_example`-style names, the right
   `OMS_TENANT`, and the SA wired through. **Do not add a CI step that needs kubectl** — see
   ground truth #7.

## Task 2 — deliver it the way this estate deploys

**Files:** `kanz/infra/gitops/applicationset.yaml`, `kanz/infra/onboarding/provision-tenant.sh`.

1. Add a **git-directory generator** to the ApplicationSet so `kanz/infra/tenants/*` yields one
   Application per tenant directory. This matches the file's own stated philosophy — *"Adding a
   component = one list entry; adding an environment = one list entry"* — extended to tenants:
   **adding a tenant = adding a directory.** Keep `goTemplate: true`. Exclude `_example`.
2. Add a **`compute` step** to `provision-tenant.sh` (the step list is `storage · infra · policy ·
   seed · verify`; place it where a reader would expect it, and say why in a comment). The step:
   - **generates** `infra/tenants/<tenant>/` from the `_example` shape,
   - prints exactly what the operator must do next: **commit it**. It must be unmistakable that
     the commit — not the script — is what deploys.
   - **REFUSES, loudly, to `kubectl apply`.** State the reason in the script where the next
     engineer will look for the apply: ArgoCD `prune: true` would delete it.
   - Follows the script's existing REFUSE-don't-guess stance (see its `RISK_DB_NAME` handling: no
     defaults, name every missing prerequisite at once, exit non-zero).
3. Add the NATS user for `spiffe://kanz.internal/ns/kanz-services/sa/oms-<tenant>` to the tenant's
   account in `infra/nats/tenancy.yaml` via the existing tenantctl path — **read how tenantctl
   appends an account/user today and follow it exactly.** Without this the pod cannot connect
   (SEC-M3). If tenantctl cannot express it, say so and STOP rather than inventing a second
   mechanism.

## Task 3 — the guard: a tenant with an account and no compute

**File:** `kanz/test/arch/tenant_compute_test.go` (new), package `arch`. **Pure Go — no kubectl.**

**Why:** `tenancy.yaml` lists tenant accounts; `infra/tenants/` lists tenant compute. **Nothing
compares them.** A tenant provisioned with streams and no pods is exactly the bug this task
exists to close, and it would silently return the first time someone adds an account by hand.

1. Assert **both directions**: every tenant account in `tenancy.yaml` (excluding `__system__` and
   `$SYS`) has an `infra/tenants/<tenant>/` overlay; every overlay has an account. Name the
   missing side in the failure and say what to do about it.
2. Assert every overlay's NATS user matches the SPIFFE ID its rendered SA would produce —
   `spiffe://kanz.internal/ns/kanz-services/sa/oms-<tenant>`. A mismatch here is SEC-M3 all over
   again: the pod exists and cannot connect.
3. **Non-vacuous:** zero tenants found, or zero overlays found, is a FAILURE, not a pass. This
   repo has shipped false-green guards twice (SEC-M5's suffix grouping, ONBOARD-M1's echo
   blacklist defeated by `printf`). Use positive matches, not a blacklist.
4. Parse the YAML — do not grep. `tenancy.yaml` discusses tenants in prose; a grep over a file
   that documents what it declined to do over-reports (that is the `internal/integrity` lesson).

**Mutation-test it and paste the output:** add an account with no overlay → FAILS naming it;
add an overlay with no account → FAILS naming it; break a SPIFFE ID → FAILS. Restore with
**targeted edits — never `git checkout -- .`** (it has destroyed uncommitted work here twice).

## Verification (EXECUTE, paste real output)

- `kubectl kustomize kanz/infra/tenants/_example/` rendered output, verbatim.
- The three Task-3 mutations, each failing, then green.
- `go test ./test/arch/... -count=1` and the full suite: `go test ./... -count=1`.
- `gofmt -l .` and `go vet ./...` clean.
- `git status --short` shows no unintended modifications.
