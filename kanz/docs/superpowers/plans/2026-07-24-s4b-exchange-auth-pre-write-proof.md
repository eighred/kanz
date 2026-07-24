# S4b — Shared exchange-auth + pre-write account proof — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Design:** `docs/superpowers/specs/2026-07-24-s4b-exchange-auth-pre-write-proof-design.md`

**Goal:** Before the operator writes a venue API key set, ask the exchange whether the keys
authenticate and which account they belong to. Refuse the write if they don't.

**Architecture:** A new shared `internal/venueadapter/exchangeauth` package owns, once per
venue, the signature scheme and the account-id endpoint. The venue REST clients delegate to
it (removing their duplicate HMAC code); the operator uses it through a small
`internal/venueproof` wrapper wired at the composition root behind a `VenueProver` seam on
the gRPC server. Unconfigured ⇒ S4a behaviour, unchanged.

**Tech Stack:** Go (stdlib crypto/net-http only — no new dependency), `operator.v1` (buf),
`k8s.io/client-go`, `github.com/charmbracelet/bubbletea`.

## Global Constraints

Every task's requirements implicitly include this section.

- **Go toolchain** `go 1.26.1` / `toolchain go1.26.5`; all `go` commands from `kanz/` with
  `GOFLAGS=-mod=mod`. **No `make`** — use `buf generate`; set
  **`GOTMPDIR="$(pwd)/.gotmp"`** for every `go test`.
- Generated protobuf imports from `github.com/kanz-eng/kanz-schemas-go/operator/v1`;
  regenerate with `cd kanz-schemas && buf generate`. **Commit the `.proto` only** — `gen/`
  is git-ignored by design (S4a precedent, commit `07f1da0`).
- **NO NEW DEPENDENCY.** `go.mod` / `go.sum` must be byte-identical at the end of this
  slice. No vendor exchange SDK, no `hashicorp/vault` SDK, no `crypto/ssh` in the operator
  (`TestNoSSHPlane` stays green and unchanged).
- **The write-only invariant from S4a is unweakened.** Key material is never logged, never
  returned in a response, never placed in an error string, never retained after submit. The
  proof path must not become the leak S4a avoided: an exchange's raw response body is NEVER
  forwarded to the caller — map failures to a fixed set of sanitized reasons.
- **`exchange_account_id` is the only new value that crosses back to the client.** It is a
  public fact (already stamped on every OrderState/Fill), not key material.
- **Order at the handler is validate → prove → write.** A failed proof performs **no write**;
  any previously stored key set is untouched.
- **No per-request bypass.** Proof is deploy-time configuration only; no request field, no
  flag, no header may disable it.
- **Proof is off unless configured**, and when on, requires an **explicit per-venue base
  URL** — there is no default endpoint. Enabled with no URL for that venue ⇒ refuse the
  write (`FailedPrecondition`), never write unproven.
- **One signer per venue.** After Task 3 the OKX HMAC/base64 header construction and the
  Binance query HMAC exist in exactly one place each. Do not leave a second copy behind.
- **Existing venue-adapter behaviour is preserved exactly** through the delegation: same
  weight-bucket accounting (OKX 1, Binance 10), same `execution.ErrRateLimited` /
  `execution.ErrEgressDenied` / `*execution.APIError` returns, same "no uid ⇒ error, never
  empty string" rule. The existing venue tests must pass **unmodified**.
- `govulncheck ./...` stays 0-reachable; `go test ./test/arch/...` stays green;
  `render()` in the TUI stays pure.

---

### Task 1: `operator.v1` — `exchange_account_id` on `SetVenueKeysResponse`

**Files:**
- Modify: `kanz-schemas/proto/operator/v1/operator.proto`
- Regenerates (not committed): `operator.pb.go`

**Interfaces:**
- Produces: `SetVenueKeysResponse.exchange_account_id` (string, field 1).

- [ ] **Step 1: Add the field**

`SetVenueKeysResponse` is currently empty. Give it exactly one field:

```proto
message SetVenueKeysResponse {
  // exchange_account_id is the exchange's own id for the account these credentials
  // spend, as the exchange itself reported it during the pre-write proof (S4b).
  // Empty when the deployment has no proof configured — the keys were written
  // unproven. It is a public fact (it is what a venue account label binds TO), never
  // key material.
  string exchange_account_id = 1;
}
```

- [ ] **Step 2: Lint + generate**

Run: `cd kanz-schemas && buf lint && buf generate`
Expected: lint exit 0; `GetExchangeAccountId()` appears in the generated Go.

- [ ] **Step 3: Commit the proto only**

`git add kanz-schemas/proto/operator/v1/operator.proto` — do **not** add `gen/`.

**Verification:** `cd kanz && GOFLAGS=-mod=mod go build ./...` succeeds against the
regenerated SDK.

---

### Task 2: `exchangeauth` — the shared signer + account-id call

**Files:**
- Create: `kanz/internal/venueadapter/exchangeauth/exchangeauth.go`
- Create: `kanz/internal/venueadapter/exchangeauth/okx.go`
- Create: `kanz/internal/venueadapter/exchangeauth/binance.go`
- Create: `kanz/internal/venueadapter/exchangeauth/exchangeauth_test.go`

**Interfaces (this is the contract every later task codes against):**

```go
// Package exchangeauth is the one place that knows how to sign an exchange REST call
// and ask the exchange which account a credential belongs to.

// Credential is a candidate exchange API key set. It is an input only: no function
// here logs it, returns it, or puts it in an error.
type Credential struct {
	APIKey     string
	APISecret  string
	Passphrase string // OKX only; empty for Binance
}

// Options tunes one call. BaseURL is required; the rest default.
type Options struct {
	BaseURL    string           // e.g. "https://www.okx.com" — required
	HTTPClient *http.Client     // default: &http.Client{Timeout: 10 * time.Second}
	Now        func() time.Time // default: time.Now
}

// ErrUnsupportedVenue names a venue exchangeauth cannot prove.
var ErrUnsupportedVenue = errors.New("exchangeauth: unsupported venue")

// Venues returns the venue ids this package can prove, sorted.
func Venues() []string

// AccountID asks the venue which exchange account cred belongs to, returning the
// exchange's own id for it. An exchange response that carries no id is an error,
// never "" — an empty id would sail upstream looking like a verified account.
func AccountID(ctx context.Context, venue string, cred Credential, opt Options) (string, error)

// SignOKX sets the OK-ACCESS-* headers for one request on h. ts is the request
// timestamp, body the JSON payload ("" for GET).
func SignOKX(h http.Header, cred Credential, ts time.Time, method, requestPath, body string)

// SignBinance returns the hex HMAC-SHA256 of query under secret — the value Binance
// expects as the &signature= parameter.
func SignBinance(secret, query string) string
```

- [ ] **Step 1 (RED): tests first**

`exchangeauth_test.go`, driving both venues against `httptest.NewServer`:

- `TestAccountIDOKX` — server asserts `OK-ACCESS-KEY`, `OK-ACCESS-PASSPHRASE`,
  `OK-ACCESS-TIMESTAMP` (format `2006-01-02T15:04:05.000Z`) and that `OK-ACCESS-SIGN`
  equals an independently computed `base64(HMAC-SHA256(ts+"GET"+"/api/v5/account/config"))`;
  path is `/api/v5/account/config`; returns `{"code":"0","data":[{"uid":"4711"}]}` ⇒ `"4711"`.
- `TestAccountIDBinance` — server asserts `X-MBX-APIKEY`, a `timestamp` and `recvWindow`
  query param, and a `signature` equal to an independently computed
  `hex(HMAC-SHA256(query))` over the query **without** `&signature=`; path
  `/api/v3/account`; returns `{"uid":8822}` ⇒ `"8822"`.
- `TestAccountIDRejectsMissingUID` (table, both venues): OKX `{"code":"0","data":[]}`,
  OKX `{"code":"0","data":[{"uid":""}]}`, Binance `{"uid":0}` ⇒ error, and the returned
  string is `""` **with** a non-nil error (assert both).
- `TestAccountIDMapsAuthFailure` (table, both venues): HTTP 401 and HTTP 403 ⇒
  `errors.Is(err, execution.ErrEgressDenied)`.
- `TestAccountIDTypedError`: OKX `{"code":"50111","msg":"Invalid API key"}` and Binance
  `{"code":-2015,"msg":"Invalid API-key"}` ⇒ `*execution.APIError` via `errors.As`.
- `TestAccountIDUnsupportedVenue`: `errors.Is(err, ErrUnsupportedVenue)`.
- `TestNoCredentialInErrors`: for every failure case above, assert the error string
  contains neither the api key nor the secret nor the passphrase (use distinctive
  sentinel values like `"KEYSENTINEL"`, `"SECSENTINEL"`, `"PASSENTINEL"`).
- `TestVenues`: exactly `["binance","okx"]`.

Run and confirm they fail to compile (undefined symbols) — that is RED.

- [ ] **Step 2 (GREEN): implement**

- `exchangeauth.go`: `Credential`, `Options` (+ an unexported `resolve()` applying the
  defaults), `ErrUnsupportedVenue`, `Venues()`, `AccountID` dispatching on venue, and a
  shared `do(req, client)` that reads at most `1<<20` bytes, maps 401/403 to
  `fmt.Errorf("%w: %s status %d", execution.ErrEgressDenied, path, code)` and `>=500` to a
  plain status error — the same shape the venue clients use today.
- `okx.go`: `SignOKX` + `okxAccountID` (GET `/api/v5/account/config`, decode
  `{code,msg,data[].uid}`, `code != "0"` ⇒ `*execution.APIError`).
- `binance.go`: `SignBinance` + `binanceAccountID` (GET `/api/v3/account` with
  `timestamp`+`recvWindow=5000`, decode `{uid,code,msg}`, `code != 0` ⇒
  `*execution.APIError`, `uid == 0` ⇒ error).

Rate limiting is **not** this package's job — callers that have a weight budget spend it
before delegating.

**Verification:**
`GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./internal/venueadapter/exchangeauth/... -count=1`
— all green. Report the count.

---

### Task 3: Venue adapters delegate — one signer per venue

**Files:**
- Modify: `kanz/services/venue-okx/internal/okx/okx_rest.go`
- Modify: `kanz/services/venue-binance/internal/binance/binance_rest.go`
- Possibly modify: the venues' `bridge.go` (only if an alias is needed)

**Interfaces:** consumes Task 2's `SignOKX`, `SignBinance`, `AccountID`.

- [ ] **Step 1: OKX**

- `signedRequest` builds its prehash/HMAC/base64 headers via `exchangeauth.SignOKX` instead
  of its own `hmac.New(...)` block. Delete the now-dead `crypto/hmac`, `crypto/sha256`,
  `encoding/base64` imports.
- `ExchangeAccountID` keeps its `bucket.Allow(1)` / `onThrottle` / `ErrRateLimited` guard,
  then returns `exchangeauth.AccountID(ctx, "okx", exchangeauth.Credential{...},
  exchangeauth.Options{BaseURL: c.baseURL, HTTPClient: c.httpc, Now: c.now})`. The local
  `okxAccountConfig` type and its decode go away.

- [ ] **Step 2: Binance**

- `sign(query)` becomes a one-line call to `exchangeauth.SignBinance(c.apiSecret, query)`.
- `ExchangeAccountID` keeps `bucket.Allow(10)` / `onThrottle`, then delegates to
  `exchangeauth.AccountID(ctx, "binance", ...)`. The `uid` field on `accountInfo` and the
  `strconv.FormatInt` decode go away **only if** `account()` still compiles without them —
  `account()` is also used for balances, so keep `accountInfo` and drop only what becomes
  unused.

- [ ] **Step 3: prove nothing changed**

The existing venue tests (`okx_account_test.go`, `binance_account_test.go`, and every other
test in those packages) must pass **without being edited**. If a test needs editing to pass,
the delegation changed behaviour — fix the delegation, not the test.

**Verification:**
`GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./services/venue-okx/... ./services/venue-binance/... -count=1`
— green, with `git diff --stat` showing **no** test file modified. Report both.

---

### Task 4: Operator — pre-write proof

**Files:**
- Create: `kanz/services/operator/internal/venueproof/venueproof.go`
- Create: `kanz/services/operator/internal/venueproof/venueproof_test.go`
- Modify: `kanz/services/operator/internal/config/config.go`
- Modify: `kanz/services/operator/internal/grpcsrv/server.go`
- Modify: `kanz/services/operator/internal/grpcsrv/server_test.go`
- Modify: `kanz/services/operator/cmd/operator/main.go`
- Modify: `kanz/infra/deploy/operator-deploy.yaml` (env only — the NetworkPolicy is Task 5)

**Interfaces:**

```go
// grpcsrv — the seam, alongside Provisioner / NodeOps / secrets.Store.
//
// VenueProver asks the exchange which account a candidate credential belongs to,
// before it is stored. Optional — nil disables pre-write proof (S4a behaviour).
type VenueProver interface {
	ProveAccount(ctx context.Context, venue string, keys secrets.VenueKeys) (string, error)
}
func (s *Server) WithVenueProof(p VenueProver) *Server

// venueproof — the implementation.
type Prover struct{ ... }
// New returns a Prover that proves the venues present in baseURLs (venue id → base URL).
func New(baseURLs map[string]string, httpc *http.Client) *Prover
// ErrNoEndpoint: proof is enabled but this venue has no base URL configured.
var ErrNoEndpoint = errors.New("venueproof: no exchange endpoint configured for this venue")
```

- [ ] **Step 1: config**

Add to `Config` and `Load()`:

```go
// Pre-write venue-key account proof (S4b). VenueProof "require" turns it on;
// empty/"off" leaves S4a behaviour (write without proof). A venue with no base URL
// configured cannot be proved, so with proof on its write is refused, never written
// unproven. There is deliberately NO default endpoint: a wrong default would either
// reject valid production keys (testnet) or send a rig's dummy keys to the real
// exchange (mainnet).
VenueProof     string            // OPERATOR_VENUE_PROOF: "" | "off" | "require"
VenueBaseURLs  map[string]string // OPERATOR_OKX_BASE_URL, OPERATOR_BINANCE_BASE_URL
```

`VenueBaseURLs` only contains the venues whose env var is non-empty.

- [ ] **Step 2 (RED): venueproof tests**

`venueproof_test.go` against `httptest`:
- `TestProveAccountReturnsUID` (okx and binance).
- `TestProveAccountNoEndpoint` — venue absent from `baseURLs` ⇒ `errors.Is(err, ErrNoEndpoint)`.
- `TestProveAccountUnsupportedVenue`.
- `TestProveAccountPropagatesAuthFailure` — 401 ⇒ error (assert it is non-nil and carries
  no credential sentinel).

- [ ] **Step 3 (GREEN): venueproof**

`Prover.ProveAccount` looks up the base URL, returns `ErrNoEndpoint` when missing, else
delegates to `exchangeauth.AccountID` with `exchangeauth.Credential{APIKey: keys.APIKey,
APISecret: keys.APISecret, Passphrase: keys.Passphrase}`. It is a ~25-line file: it maps
the operator's types onto Task 2's and adds the endpoint lookup. Nothing else.

- [ ] **Step 4 (RED): handler tests**

In `server_test.go`, with a stub prover:
- `TestSetVenueKeysProvesBeforeWrite` — success: response `ExchangeAccountId` is the
  prover's uid, and the store received the write.
- `TestSetVenueKeysRefusesWriteWhenProofFails` — prover errors ⇒ `codes.FailedPrecondition`
  **and the store received NO write** (assert on the fake store's call count).
- `TestSetVenueKeysProofErrorIsSanitized` — the prover returns an error whose text contains
  a credential sentinel and a fake exchange body; assert the gRPC message contains
  **neither**, and that it is one of the fixed reasons.
- `TestSetVenueKeysWithoutProverWritesUnproven` — nil prover ⇒ write happens, response
  `ExchangeAccountId` is `""` (S4a behaviour intact).
- Order: `TestSetVenueKeysValidatesBeforeProving` — an invalid key set ⇒ `InvalidArgument`
  and the prover was never called.

- [ ] **Step 5 (GREEN): handler**

In `SetVenueKeys`, between validation and the store write:

```go
var accountID string
if s.venueProof != nil {
	id, err := s.venueProof.ProveAccount(ctx, req.GetVenue(), keys)
	if err != nil {
		// Sanitized: the exchange's own words never reach the caller, and neither
		// does the credential. No write happens — the stored key set is untouched.
		return nil, status.Error(codes.FailedPrecondition, proofReason(err))
	}
	accountID = id
}
```

`proofReason(err)` maps to a **fixed** set of strings, chosen by `errors.Is`/`errors.As`,
with a catch-all — for example:
- `venueproof.ErrNoEndpoint` → `"venue key proof is required but no exchange endpoint is configured for this venue"`
- `execution.ErrEgressDenied` → `"the exchange rejected these credentials"`
- `*execution.APIError` → `"the exchange rejected these credentials (code N)"` — the numeric
  code only, never `Msg`
- `exchangeauth.ErrUnsupportedVenue` → `"this venue cannot be proved"`
- default → `"the exchange could not be asked which account these credentials belong to"`

Return `&operatorpb.SetVenueKeysResponse{ExchangeAccountId: accountID}` after the write.

- [ ] **Step 6: wiring**

In `main.go`, after the secret backend switch: when `cfg.VenueProof == "require"`, build
`venueproof.New(cfg.VenueBaseURLs, execution.NewExchangeHTTPClient(5*time.Minute))` and
`srv = srv.WithVenueProof(...)`; log the enabled venues (**ids only**). Log
`"venue-key pre-write proof: off"` otherwise. Add the three env vars (commented, unset by
default) to `infra/deploy/operator-deploy.yaml`.

**Verification:**
`GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./services/operator/... -count=1` — green.
Report the count and confirm the no-write-on-failure assertion is present.

---

### Task 5: Operator egress NetworkPolicy + arch guard

**Files:**
- Modify: `kanz/infra/deploy/operator-deploy.yaml`
- Create: `kanz/test/arch/operator_egress_test.go`

**Interfaces:** none (deployment + guard).

- [ ] **Step 1: the policy**

Add an `operator-egress` NetworkPolicy in `kanz-operator`, `podSelector: app: operator`,
`policyTypes: [Egress]`, following the existing `node-provisioner-egress` style:
- DNS: UDP+TCP `:53` to the kube-dns namespace/pod selector already used by
  `node-provisioner-egress`.
- Exchange: TCP `:443` to `ipBlock: cidr: 0.0.0.0/0` with `except:` `10.0.0.0/8`,
  `172.16.0.0/12`, `192.168.0.0/16`, `169.254.0.0/16` — public internet only, so this new
  permission cannot be turned inward at in-cluster services or the cloud metadata endpoint.
- Nothing else. Comment it as S4b's pre-write proof egress and say why the `except` list is
  the point.

- [ ] **Step 2: the guard**

`operator_egress_test.go`, modelled on `operator_venue_secret_rbac_test.go`:
- `TestOperatorEgressIsBounded` — parse `operator-deploy.yaml`, find `operator-egress`,
  assert: `policyTypes == [Egress]`; ports are exactly `{53/UDP, 53/TCP, 443/TCP}`; the 443
  rule's `ipBlock.except` contains all four private/link-local CIDRs; there is **no** rule
  with an empty `to:` (which would be unrestricted egress).
- `TestOperatorEgressGuardIsNonVacuous` — the guard fails if the policy is missing (assert
  the lookup returns a not-found error path, or run the parse against a stripped copy).

- [ ] **Step 3: pin the import boundary**

`exchangeauth` exists because Go's `internal` rule makes
`services/venue-okx/internal/...` unimportable from `services/operator` — that boundary is
currently protected by nothing but the compiler's willingness to enforce it today. Add
`TestOperatorImportsNoVenueService` to the same file (or alongside the existing import-graph
guards in `test/arch/ssh_plane_test.go` / `risk_boundary_test.go` — follow whichever pattern
those use): walk the operator's transitive imports and assert none is under
`services/venue-`. Mutation-prove it the same way as the egress guard.

**Verification:** `GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./test/arch/... -count=1`
— green. Then **mutation-prove the guard**: delete one `except` CIDR, re-run, confirm it
fails naming that CIDR, restore, confirm green. Report all three outcomes.

---

### Task 6: TUI — show the proof result

**Files:**
- Modify: `kanz/cmd/universe/model.go` (API-Manager pane + masked key-entry form)
- Modify: `kanz/cmd/universe/source.go` (the operator client seam that calls `SetVenueKeys`)
- Modify: `kanz/cmd/universe/model_test.go`

**Interfaces:** consumes `SetVenueKeysResponse.ExchangeAccountId` and
`codes.FailedPrecondition`.

- [ ] **Step 1: success**

On a successful submit whose response carries a non-empty `ExchangeAccountId`, the pane
shows `✓ verified · account <id>` for that venue. Empty id ⇒ the existing `✓ configured`
wording, unchanged (unproven deployments must not start claiming verification).

- [ ] **Step 2: rejection**

On `FailedPrecondition`, show the server's (already-sanitized) message inline on the form
and **keep the form open with every field cleared** — the operator retypes. The key material
must not be retained to be re-submitted; that is the S4a invariant, and a rejected key set is
exactly the one you least want held in memory.

- [ ] **Step 3: tests**

Extend the existing form/pane tests: a proved submit renders the account id; a
`FailedPrecondition` submit renders the reason, leaves the form open, and leaves **no**
field holding the typed secret (assert on the model, not the render). `render()` stays pure.

**Verification:** `GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./cmd/universe/... -count=1`
— green. Report the count.

---

### Task 7: Whole-slice verification + rig-proof handoff

**Files:** none (verification only; report to `docs/…/task-7-report.md`).

- [ ] **Step 1: full build + test**

```
cd kanz
GOFLAGS=-mod=mod go build ./...
GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./... -count=1
GOFLAGS=-mod=mod govulncheck ./...
git diff --stat -- go.mod go.sum      # MUST be empty
```

- [ ] **Step 2: invariant sweep**

Grep-prove, and quote the evidence:
- no `ExchangeAccountID`-style HMAC construction remains outside `exchangeauth`
  (`grep -rn "hmac.New" services/venue-*/ internal/venueadapter/`);
- no response, log line or error string in the operator carries `APIKey`/`APISecret`/
  `Passphrase` (`grep -rn "APISecret\|Passphrase" services/operator/`);
- `TestNoSSHPlane` and the S2a/S3/S4a guards are green and **unchanged**.

- [ ] **Step 3: rig-proof recipe**

Write the exact commands to prove the reject path on kind (no real exchange needed): point
`OPERATOR_OKX_BASE_URL` at a stub that returns 401, `OPERATOR_VENUE_PROOF=require`, enter
dummy keys in the TUI, observe the inline rejection, and confirm with `kubectl get secret -n
kanz-services venue-okx-keys` that **nothing was written**. Then the accept path against a
stub returning a uid, confirming the secret appears and the pane reads
`✓ verified · account <id>`.

**Verification:** every command above run and its real output reported. No claim without output.

---

## Notes for the executor

- Tasks 2→3 are the extraction; 4→6 are the operator/TUI slice; 5 is deployment. Task 3 is
  the one that can regress live trading — its gate is "existing venue tests pass unmodified".
- The temptation in Task 4 is to reuse `accountproof.Resolve`. Don't: it answers "is this the
  account we CLAIM?", and the operator has no claim to check against at key-entry time. It
  stays adapter-only and unchanged.
- If any task's review surfaces a conflict with these constraints, stop and escalate — the
  write-only invariant is not a preference.
