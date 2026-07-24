# API Manager — operator-plane venue-key write path (S4a)

**Date:** 2026-07-24
**Status:** design approved, pending implementation plan
**Direction:** the API-Manager slice of the TUI-based sovereign fund-management surface, after S3 (cluster ops). Lead-committed 2026-07-24.

## Context

The mockup drew an **API Manager**: register exchange API keys (key, secret, and
for OKX a passphrase) per exchange. Most of that path is already built and the
ground truth reshapes the slice:

- **The consume side already exists.** `venue-okx` and `venue-binance` are real
  HMAC-signing REST clients that read their credentials via a `secret()` file-mount
  reader (`<KEY>_FILE` → CSI/Vault tmpfs, plaintext-env fallback) and **prove the
  key's exchange account `uid` against the live exchange at boot**, refusing to serve
  orders on a mismatch (SOV-02a, `internal/venueadapter/accountproof`). The Vault
  paths already exist in `infra/security/secrets/secretproviderclass.yaml` —
  `kv/data/kanz/venue-okx` (`api_key`/`api_secret`/`api_passphrase`) and
  `kv/data/kanz/venue-binance` (`api_key`/`api_secret`).
- **The write side is the gap.** There is **no programmatic Vault write anywhere** —
  no `hashicorp/vault` dependency; every key today is a human typing `vault kv put`
  from `infra/security/secrets/README.md`. The operator service holds zero credential
  handling (its config comment states "no write target, no credential"), and the
  `operator.proto` header anticipates this slice: *"key material arrive[s] in later
  subsystems as their own RPCs."*

So **S4a = the operator-plane write path that puts a venue key set into its secret
location** — the missing write half of an otherwise-complete consume path.

Two forks were resolved during design:

1. **The rig has no Vault** (the ONBOARD-M6 gap). The operator writes through a
   **`SecretStore` seam**: a Vault impl for production, a k8s-Secret impl for the rig.
   This mirrors the platform's existing, guarded Vault-CSI → committed-dev-Secret
   substitution (`test/arch/rig_dev_posture_test.go`), keeping S4a rig-provable on
   kind. Rejected: a Vault-only path (unverifiable here, ships "implemented but
   unproven", BLOCKED-on-Vault exactly like ONBOARD-M6).
2. **Pre-write account-proof is deferred to S4b.** Validating a candidate key against
   the live exchange before it lands requires a signed exchange call, and the signers
   live inside each venue service's `internal/` (un-importable) entangled with the
   trading connector — so proof requires extracting a shared exchange-auth client, a
   refactor of the live money path. That is its own focused slice (S4b), exactly as
   the S2 write path (S2a) shipped before the Test Connection probe (S2b). **S4a is
   write-only** and relies on the venue adapter's existing boot-time account-proof as
   the safety net (a bad key fails the adapter at boot, not the write).

## Security posture (the spine of the slice)

Exchange keys move real capital; BRAIN records they "must never be reachable from a
request path." So the operator's new capability is **write-only, scoped, and never
read-back**:

- The `SecretStore` interface has **no method that returns key material.** Presence
  (`ListVenues`) returns only `{venue, configured}`. This is the enforceable
  write-only invariant — interface shape + code + arch guard.
- The operator **never logs or returns** an api_key/secret/passphrase on any path
  (test-enforced, as with the S2a Job-owned Secret).
- The k8s-Secret backend's RBAC is **namespace-bounded** to `kanz-services` and the
  `secrets` resource only; the Vault backend's token is **write-scoped** to the
  `kv/data/kanz/venue-*` prefix (prod).
- The TUI **masks** the typed secret/passphrase (`••••`), never echoes it, and holds
  it only in the form field until submit, then clears it from the model (the typed
  secret is never retained after the RPC — the S2a key-bytes discipline).
- No `crypto/ssh` (this is HTTP + the k8s API); `TestNoSSHPlane` stays green.

## Scope

**In:**

1. **`operator.v1.SetVenueKeys` / `ListVenueKeys`** RPCs (additive to the proto).
2. **`SecretStore` seam** (`services/operator/internal/secrets`): the value-blind
   interface + a **k8s-Secret** impl (rig/dev) + a dependency-free **Vault KV-v2**
   impl (prod).
3. **gRPC handlers** on the operator `Server` (a `Store` field + `WithSecrets`
   builder; nil → Unimplemented), with venue/field validation.
4. **RBAC**: a bounded `operator-venue-secret-writer` Role + binding, plus the arch
   guard bounding it.
5. **TUI**: an API-Manager pane listing configured venues (presence only) + a masked
   key-entry form that fires `SetVenueKeys`.

**Out, recorded rather than assumed away:**

- **Pre-write account-proof + the shared exchange-auth extraction** — S4b.
- **Key rotation / deletion / multi-account-per-venue** — the manual runbook still
  covers rotation; S4a establishes the write path, not the full lifecycle.
- **A live Vault write and a live venue adapter consuming the Secret** — infra-gated
  (no Vault on the rig; the real venue adapters are not in the rig loop subset, which
  runs the in-process sim venue). The Vault impl is unit-proven against an `httptest`
  Vault; the k8s-Secret write is fully rig-provable.
- **Reading a key back** — never a use case; the interface forbids it by shape.

## Design

### 1. `operator.v1` proto

```
rpc SetVenueKeys(SetVenueKeysRequest) returns (SetVenueKeysResponse);
rpc ListVenueKeys(ListVenueKeysRequest) returns (ListVenueKeysResponse);

message SetVenueKeysRequest {
  string venue      = 1; // "okx" | "binance"
  string api_key    = 2;
  string api_secret = 3;
  string passphrase = 4; // OKX only; empty for Binance
}
message SetVenueKeysResponse {}

message ListVenueKeysRequest {}
message ListVenueKeysResponse { repeated VenueKeyStatus venues = 1; }
message VenueKeyStatus {
  string venue      = 1;
  bool   configured = 2; // presence only — NEVER key material
}
```

Additive; `buf breaking` clean.

### 2. `SecretStore` seam — `services/operator/internal/secrets`

```go
// VenueKeys is a candidate key set. It is passed to a backend and never returned.
type VenueKeys struct{ APIKey, APISecret, Passphrase string }

// VenueStatus is the value-blind presence report.
type VenueStatus struct {
    Venue      string
    Configured bool
}

// Store is the write-only venue-credential surface. It has NO method that returns
// key material — presence is the only read. Both backends satisfy it.
type Store interface {
    SetVenueKeys(ctx context.Context, venue string, keys VenueKeys) error
    ListVenues(ctx context.Context) ([]VenueStatus, error)
}
```

Field validation lives at the handler (see §3) so both backends see a well-formed
call; a backend may also guard defensively. The known venues and their required
fields are a single source of truth in this package:

- `okx`  → api_key, api_secret, **passphrase all required**
- `binance` → api_key, api_secret required, **passphrase must be empty**

**k8s-Secret backend (rig/dev).** Server-side-*applies* a Secret named
`venue-<venue>-keys` in namespace `kanz-services`, `type: Opaque`, with data keys
`api-key` / `api-secret` / `api-passphrase` (matching the SecretProviderClass
**objectName**/mounted-filename convention — hyphenated — since the dev rig mounts
this Secret as a plain volume with no CSI remap; the Vault backend, by contrast,
writes the Vault `secretKey` convention, underscored). Apply
(`types.ApplyPatchType`, a fixed field-manager) is create-or-update in one call with
**no read-back of values**. `ListVenues` reads Secret **metadata only** (existence by
name) and never surfaces `.Data`. The operator's existing client-go and in-cluster SA
are reused; the write needs the S4a Role (§4).

- *Accepted limit, documented:* k8s Secret RBAC cannot express "presence without
  value" — `get`/`list` on secrets returns data on the wire. The write-only guarantee
  is therefore enforced by the interface (no value-returning method), the code
  (`ListVenues` reads only `ObjectMeta`, the operator never logs `.Data`), and the
  namespace bound — not by RBAC alone. The Vault backend does not share this limit.

**Vault backend (prod).** A **dependency-free** KV-v2 writer — an authenticated HTTP
PUT to `{VAULT_ADDR}/v1/kv/data/kanz/venue-<venue>` with an `X-Vault-Token` header and
a `{"data": {...}}` body — matching the codebase's hand-rolled, no-vendor-SDK HTTP
convention (`okx_rest.go`, `binance_rest.go`: "no vendor SDK is imported"), keeping the
dependency tree and `govulncheck` clean. `ListVenues` issues KV-v2 `LIST`
(`GET .../v1/kv/metadata/kanz?list=true`), which returns key **names, not values** —
truly value-blind. Config: `VAULT_ADDR`, a write-scoped token via
`VAULT_TOKEN_FILE`/env. Unit-tested against an `httptest` server asserting the request
method, path, token header, and body shape; a live Vault is infra-gated.

Which backend the operator uses is a startup config choice
(`OPERATOR_SECRET_BACKEND=kube|vault`); `main.go` wires one and calls
`WithSecrets(store)`. An operator with no backend configured leaves the `Store` nil →
the handlers return `Unimplemented`.

### 3. Operator — gRPC handlers

Mirror S3's `nodeOps` shape. `Server` gains a `secrets Store` field and a
`WithSecrets(Store) *Server` builder (composes with the existing constructors). A
shared guard maps the errors:

- `secrets == nil` → `codes.Unimplemented` ("API management not configured").
- Unknown venue, missing required field, or a passphrase supplied for Binance / absent
  for OKX → `codes.InvalidArgument` (checked before the backend call, so a malformed
  key set never reaches a store).
- A backend write error → `codes.Internal` (the error text is the backend's; it must
  never contain key material — the backends are written not to echo the value).

`SetVenueKeys` validates then calls `Store.SetVenueKeys`; `ListVenueKeys` maps
`[]VenueStatus` to the proto. **Neither handler logs the request's key material** —
test-enforced.

### 4. RBAC + deploy

A namespaced Role `operator-venue-secret-writer` in `kanz-services`:

```yaml
rules:
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["create", "patch", "get", "list"]   # get/list are the presence read; namespace-bounded
```

Bound to the operator SA by a RoleBinding. **Not cluster-scoped; `secrets` only; no
other resource or verb.** `test/arch/operator_venue_secret_rbac_test.go`
(`TestOperatorVenueSecretRoleIsBounded`) fails the build on any wider grant — a verb
beyond the four, a resource other than `secrets`, or a ClusterRole. Mutation-proven
(inject `delete` or `nodes` → build fails). The Vault backend needs no k8s RBAC (its
authority is the Vault token); it is prod-only.

### 5. TUI — API Manager pane

A third pane alongside Nodes / Clusters (`paneAPI`), reachable by `tab` cycling.

- **Presence list.** Renders `ListVenueKeys` — one row per venue with a configured
  marker (e.g. `binance  ✓ configured` / `okx  — not set`). **Never key material.**
- **Key-entry form.** A key (e.g. `k`) opens a form: a venue selector (okx/binance)
  and three fields — api_key, api_secret, passphrase (passphrase shown only when the
  venue is okx). The **api_secret and passphrase render masked** (`••••` for the typed
  length) — never echoed. Enter submits `SetVenueKeys` off the UI thread; the typed
  secret is read into the request and then **cleared from the model** (never retained
  after submit — the S2a key-bytes discipline). Esc cancels and clears. A failed
  submit sets a form error (the operator stays to retry), never a crash.
- If the backend is unconfigured (`Unimplemented`), the pane shows "API management not
  configured" rather than an empty list.
- `render` stays a **pure** function of model — no I/O, no clock.

## Testing

- **`SecretStore` — k8s impl:** fake clientset — `SetVenueKeys("binance", …)` applies
  a Secret `venue-binance-keys` in `kanz-services` with the three data keys; re-apply
  updates in place (no duplicate, no read-back error); `ListVenues` reports it
  configured; an okx set writes `api_passphrase`. Assert the Secret's data keys, not
  a mocked call.
- **`SecretStore` — Vault impl:** `httptest` server — `SetVenueKeys` issues a PUT to
  `/v1/kv/data/kanz/venue-binance` with the token header and a `{"data":{...}}` body
  carrying the three fields; `ListVenues` parses a KV-v2 LIST response into presence.
  A non-2xx Vault response → error (surfaced, no key material in the message).
- **Validation:** unknown venue, empty api_key/api_secret, passphrase-for-binance,
  missing-passphrase-for-okx each → error before any backend call.
- **Handler:** delegation (venue+fields reach the stub store); nil store →
  Unimplemented; each validation failure → InvalidArgument; a stub write error →
  Internal. A test asserts **no key material appears in any log** the handler emits.
- **TUI:** `k` opens the form; the api_secret field renders masked; Enter fires
  `SetVenueKeys` with the selected venue + typed fields (recorded in the stub) and
  clears the secret from the model; Esc cancels; the presence list renders from
  `ListVenueKeys`; `render` pure. A model carrying a typed secret is asserted to hold
  no secret after a successful submit.
- **Arch guards:** `TestOperatorVenueSecretRoleIsBounded` green + mutation-proven;
  `TestNoSSHPlane` green (no `crypto/ssh`); the S2a/S3 guards unchanged and green.
- **`govulncheck ./...`** 0-reachable (the dependency-free Vault writer adds no dep
  tree).

## Definition of done

From `universe`, the operator opens the API Manager pane, enters a key set for a
venue (secret masked), and Enter; `ListVenueKeys` then shows that venue configured,
and — on the k8s backend — `kubectl get secret -n kanz-services venue-<venue>-keys`
carries the three data keys. The `SecretStore` seam is unit-proven for both backends
(k8s against a fake clientset, Vault against `httptest`); the handler validation and
the no-key-in-logs property are proven; `render` pure; the venue-secret RBAC guard +
`TestNoSSHPlane` green; `govulncheck` 0-reachable. **The k8s-Secret write is fully
rig-provable on kind**; a live Vault write and a live adapter consuming the Secret are
infra-gated.

## Sequencing (recorded, not part of this slice)

- **S4a (this spec)** — the venue-key write path (SecretStore seam, write-only).
- **S4b** — extract a shared exchange-auth client out of the venue services and add
  **pre-write account-proof** (reject a wrong/dead/mis-scoped key at entry, reusing
  `accountproof`), the S4 analogue of S2b's Test Connection.
- **S5** — Setup Wizard (first-node bootstrap chicken-and-egg).
- **Control-plane bring-up (infra-gated)** — a k3s server + a real Vault/SPIRE rig for
  S2a's live join, S2b's addons, and S4's live Vault write.
