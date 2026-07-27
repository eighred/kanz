# Repository Analysis — Phase 0 (Inventory)

**Pinned to:** `origin/main` @ `4ca6a9a` (*Merge pull request #47 from eighred/release/pin-v0.2.0*).
**Date:** 2026-07-28 · **Method:** direct inspection. Every claim carries a `file:line`, a `git grep` against the pinned rev, or a command whose output is quoted. Where I could not find something, I name what I searched rather than asserting absence.

**Scope note.** Phase 0 is inventory only — no gap analysis, no plan, no code. Target-system requirements are referenced only where they changed *what I went looking for*.

**Concurrency caveat.** The repository moved under this analysis mid-run: `origin/main` advanced from `9021ec8` to `4ca6a9a` and local `HEAD` changed branches while the scan was in progress. Every load-bearing claim below was therefore **re-verified against the pinned rev** with `git grep <rev>`. Claims read from the working tree at `9021ec8` and not re-pinned are marked *(tree@9021ec8)*.

---

## 0. Size of the thing

| Measure | Value | How |
|---|---|---|
| Tracked files | 1,443 | `git ls-files \| wc -l` |
| Go LOC, non-test | 80,691 | `find kanz -name '*.go' \| grep -v _test \| xargs wc -l` |
| Go test files | 441 | `find kanz -name '*_test.go'` |
| Architecture guards | 38 | `ls kanz/test/arch/` |
| Services | 26 | `kanz/services/*/` |
| Binaries | 12 | `kanz/cmd/*/` |
| SQL migrations | 27, across 13 services | `ls kanz/services/*/migrations/*.sql` |
| Deploy manifests | 33 | `ls kanz/infra/deploy/` |
| `TODO`/`FIXME`/`HACK`/`XXX` in non-test Go | **6** | §4 |

`go build ./...` → **exit 0** (verified, tree@9021ec8). The module compiles.

---

## 1. Language / Runtime / Build System

| Component | Location | Notes |
|---|---|---|
| **Go module** | `kanz/go.mod` | `module github.com/kanz-eng/kanz`, `go 1.26.1`, toolchain `go1.26.5`. One module rooted at `kanz/`; 27 direct requires. |
| **Go build** | `kanz/Makefile` | `release` target uses `-trimpath -ldflags="-s -w"`. |
| **Python** | `kanz-py/pyproject.toml` | Distribution is named `kanz-bus` but ships **two** packages: `kanz_bus/` (11 modules) and `kanz_inference/` (the AI layer, 22 modules). `kanz-py/Dockerfile` builds the inference image. |
| **TypeScript** | `kanz/ts/kanz-bus/package.json` | Cross-language bus client; Node pinned by `.nvmrc`. |
| **Protobuf** | `kanz-schemas/buf.yaml` | buf-managed. Generated code is **not committed**; images generate it at build time — `kanz-py/Dockerfile`: *"an image that COPYs a generated tree silently ships whatever was last on the builder's disk."* |
| **CI** | `.github/workflows/` | 7 workflows: `kanz-ci`, `build`, `release`, `security`, `latency`, `preview`, `codeql`. |

**Load-bearing dev seam:** `kanz/go.mod` carries `replace github.com/kanz-eng/kanz-schemas-go => ../kanz-schemas/gen/go`, so every Dockerfile builds from the **repo root**, not `kanz/`.

---

## 2. Directory Map

| Path | Purpose |
|---|---|
| `kanz/` | The Go module: services, shared packages, infra manifests, tests. |
| `kanz/cmd/` | 12 standalone binaries (§3). |
| `kanz/services/` | 26 service trees, each `package main` under `cmd/<name>/`. |
| `kanz/internal/` | Shared packages promoted on a second consumer (`dec`, `execution`, `venueadapter`, `risk`, `signal`, `marketedge`, `compliance`, `collateral`, `regulatory`, `optimization`, `prediction`, …). |
| `kanz/pkg/` | Public-boundary packages (`bus`, `auth`, `transport`, `alpha`, `deviceauth`, `redisadapter`). |
| `kanz/infra/` | 16 subtrees: `deploy`, `nats`, `kafka`, `dr`, `security`, `observability`, `gitops`, `tenancy`, `onboarding`, `operator`, `lakehouse`, `chaos`, `terraform`, `catalog`, `backstage`, `messaging`. |
| `kanz/test/` | `arch/` (38 guards), `contract/`, `e2e/`, `load/`, `mtls/`. |
| `kanz-py/` | Python: bus client + the `kanz_inference` AI layer. |
| `kanz-schemas/` | Protobuf contracts + buf config. |
| `tools/` | Repo-root ops scripts: `rig-apply.sh`, `rig-images.sh`, `rig_dev_patch.py`, `validate-board.sh`. |
| `docs/` | `RELEASE_READINESS.md`, `superpowers/{specs,plans}` (49 files), `runbooks/`. |

---

## 3. Entry Points

**12 binaries under `kanz/cmd/`:** `kanz` · `universe` · `kanz-monitor` · `kanz-halt` · `kanz-migrate` · `kanz-provisioner` · `kanz-altevent` · `kanz-household` · `kanz-mandate` · `kanz-devtoken` · `kanz-pin-digests` · `kanz-tenantgen`.

### The two terminal clients are different products

**`kanz`** (`kanz/cmd/kanz/main.go:1-6`) — *"the Eighred institutional risk & portfolio terminal client: a Claude-Code-style REPL over the delivered api-gateway `/v1` edge."* Measured surface, because this is the binary the target system's §1.1 is about:

| Fact | Evidence |
|---|---|
| Total size | **544 LOC** across 4 files: `repl.go` 327, `gateway/client.go` 167, `config/config.go` 50, plus `tokenstore` |
| Rendering | **Line-oriented REPL over `os.Stdin`/`os.Stdout`** — `repl.New(cfg, store, auth, os.Stdin, os.Stdout)` (`main.go`). It does **not** import bubbletea (`grep -rln bubbletea` lists `cmd/universe/*` and `cmd/kanz-monitor/*`, never `cmd/kanz`). No sub-screens. |
| Login | **OIDC device flow** via `pkg/deviceauth` — prints a verification URL + user code, then polls (`main.go`, `deviceLogin.Login`) |
| Session persistence | `tokenstore` writes `{os.UserConfigDir}/kanz/token.json`, **0600**, atomic write-temp-then-rename (`tokenstore.go`) |
| Command set | **8**: `/quit` `/exit` `/help` `/login` `/logout` `/whoami` `/exposure` `/measures` `/scenario` (`repl.go:98-121`) |

**`universe`** (`kanz/cmd/universe/main.go:1-11`) — the **operator TUI** (bubbletea): lists the Kubernetes estate, provisions nodes, drives cordon/drain/region, manages venue API credentials. Its header records a supersession worth noting: it **used to** dial `operator.v1` in plaintext at `localhost:9090` behind a human-started `kubectl port-forward`; it now *"reaches the estate through the API gateway, and holds no cluster access of any kind (OPS-M2c)… The operator's identity is now their own bearer token."* This supersedes the mechanism described in `KANZ_BRAIN.md:159`.

**26 services** under `kanz/services/`: accounting, alternatives, api-gateway, archiver, audit, autopilot, compliance, copilot, datamaster, lake-sink, lineage, market-data, market-ingest, oms, operator, optimization, performance, regulatory, risk-engine, schema-registry, tv-sync, venue-binance, venue-okx, wealth, web-bff, webhook-ingest.

---

## 4. Existing Components — implemented vs. skeleton

**The repository is remarkably free of stubs.** All 6 `TODO`-class markers in non-test Go:

- `kanz/internal/compliance/rules.go:239,242` — a deliberate, explained `context.TODO()` (no request context reaches the rule engine yet).
- `kanz/tools/scaffold/templates.go:10,71,181,205` — **template text**, i.e. TODOs a generator emits, not TODOs in product code.

**Cluster-proven** (per `KANZ_TASKS.md` readiness rows, each carrying commit evidence — *board-sourced, not re-verified here*): the trading loop end-to-end (webhook-ingest → signal → OMS → SimVenue → fills → tv-sync projection), OMS crash-recovery reconciliation, the compliance pre-trade gate, the archiver (NATS→Kafka single writer), operator-TUI node lifecycle, NATS mTLS with SPIFFE SVID→user mapping.

**Built but not driven by an automated producer** *(tree@9021ec8)*: `alternatives` and `wealth` — their only publishers are the `kanz-altevent` / `kanz-household` operator CLIs.

**Parked by decision:** `web-bff` — the end-user product surface (PS-01…PS-07) was cancelled 2026-07-13; the service is still built.

**The AI layer is real and, as of now, wired.** `kanz-py/kanz_inference/` ships a gRPC servicer, streaming worker, point-in-time feature store, permutation-Shapley explainer, model registry with a promotion gate, shadow execution, drift trigger and model cards — 22 modules, 24 pytest files. Its own Dockerfile records that this layer once *"had NO ENTRYPOINT, NO DOCKERFILE, NO MANIFEST and NO CONCRETE MODEL, and on the Go side `internal/prediction` had zero importers outside its own tests."* **That is no longer true:** `kanz/infra/deploy/inference-deploy.yaml` exists, and `internal/prediction` now has three non-test importers — `kanz/internal/risk/engine/recompute.go`, `kanz/services/risk-engine/cmd/risk-engine/main.go`, `kanz/services/risk-engine/internal/app/features.go`.

**Dead pointers (not dead code)** *(tree@9021ec8)*: `kanz/infra/observability/alerts/data-quality.rules.yaml` and `slo/slo.recording.rules.yaml` alert on metrics emitted by the deleted `internal/integrity`. I confirmed `kanz/internal/integrity` does not exist.

**Weight distribution** — non-test LOC in the analytic packages, which matters when judging MVP scope:

```
internal/risk         12,109      internal/marketedge     1,700
internal/compliance    1,590      internal/execution      1,494
internal/regulatory    1,359      internal/optimization   1,343
internal/alternatives    683      internal/collateral       548
internal/wealth          463
```

`internal/risk` alone is ~15% of the Go codebase. `regulatory` (FRTB/DRC), `optimization` and `collateral` (SIMM, financing) total ~3,250 LOC of institutional analytics that no MVP requirement names.

---

## 5. Data Layer

**27 migrations across 13 services**: accounting (4), alternatives (2), audit (1), datamaster (3), market-data (1), oms (4), regulatory (1), risk-engine (3), schema-registry (1), tv-sync (1), venue-binance (2), venue-okx (2), wealth (2).

**Accounting ledger** — `kanz/services/accounting/migrations/0001_ledger.sql` defines `ledger_entries` + `ledger_snapshots`, **bitemporal** (`effective_time` + `knowledge_time`). `0004_ledger_worm.sql` adds an engine-level append-only trigger. Other bitemporal stores: `accounting/0003_venue_account_scope.sql`, `market-data/0001_price_history.sql`.

**`portfolios` is engine state, not a user-managed entity.** `kanz/services/risk-engine/migrations/0001_state.sql:15-30` — columns are `portfolio_id`, `display_name`, `base_currency`, `cash_balance`, `total_market_value`, `position_count`, `as_of`, `log_topic/partition/offset`. The header calls it *"one snapshot row per portfolio"* for rehydrating a restarted engine at `log_offset + 1`. There is **no allocated-capital column, no target-weight column, and no create/update path from a user**.

**"Basket" exists only as prose.** `git grep -in basket origin/main -- '**/*.go' '**/*.sql' '**/*.proto'` returns **4 hits, all comments**: `internal/execution/router.go:63`, `internal/execution/account_test.go:67-68`, `services/oms/internal/order/service.go:69` — each explaining the isolation guarantee *"Basket Alpha's drawdown cannot touch Basket Beta's collateral."* No table, no proto message, no handler.

**Money precision:** `kanz/internal/dec/dec.go:4` — *"Money, prices, quantities and L2 depth must NEVER touch float64."* Exact base-10 via `common.v1.Decimal` ↔ `*big.Rat`, enforced by `kanz/test/arch/decimal_domain_test.go` and `decimal_conversion_test.go`. Decimal-typed columns are stored as marshaled proto `BYTEA` precisely because *"there is no lossless native SQL column for them"* (`risk-engine/migrations/0001_state.sql:8-14`).

**Tenancy:** Postgres RLS with `ENABLE` + `FORCE ROW LEVEL SECURITY` and an `app.tenant_id` GUC, deny-by-default; asserted by `kanz/test/arch/tenant_scope_test.go`. Seven of the 27 migrations are named `*_tenant_scope_required.sql`.

---

## 6. Messaging / IPC

**NATS JetStream** is the live spine (short-retention hot tier); **Kafka** is the durable log of record, **derived from NATS by one archiver** — services never dual-publish. **gRPC** for venue adapters (`venue.v1.VenueAdapterService`) and the operator plane (`operator.v1`). **HTTP** at the api-gateway edge.

Bus client `kanz/pkg/bus/` (`bus.go`, `consumer.go`, `command.go`, `context.go`, `dedup.go`) exposes one `Client` interface over `NATSClient` and `KafkaClient`, so envelope stamping, lineage, dedup and DLQ layer once. Wire framing is a serialized `envelope.v1.EventFrame`. A parallel Python implementation lives at `kanz-py/kanz_bus/`, with `tests/test_serialization_parity.py` guarding cross-language framing.

---

## 7. Exchange Integrations

**Two adapters, matching MVP scope:** `kanz/services/venue-binance/` and `kanz/services/venue-okx/`.

**Idempotency is centralised, not per-adapter.** `KANZ_BRAIN.md:18` states the contract — our `order_id` **is** the venue `clOrdId` / `newClientOrderId`, so a duplicate or ambiguous-timeout submit recovers by *querying that id* rather than double-executing; `fill_id` is deterministic (`{venueSymbol}-{tradeId}`). The mechanism lives in `kanz/internal/execution/` (`venue.go`, `closes.go`, `query.go`, `grpc_venue.go`, `fixvenue.go`), not in the adapters.

### FINDING 7.1 — the per-venue build-tag split described in `KANZ_BRAIN.md` does not exist

`KANZ_BRAIN.md:17` claims *"Per-venue build-tag connectors (`//go:build binance`, `//go:build okx`, shared infra under `binance || okx`)… The **default binary is vendor-free** — it links no `coder/websocket`… the authorized websocket transport links only under an exchange tag."* Line 44 repeats it as an anti-decompilation property.

`git grep -n "go:build" origin/main -- 'kanz/**/*.go'` (non-test) returns **7 lines, and not one names `binance` or `okx`**:

```
kanz/pkg/redisadapter/goredis.go:1                                   //go:build redis
kanz/services/copilot/cmd/copilot/model_anthropic.go:1               //go:build anthropic
kanz/services/copilot/cmd/copilot/model_stub.go:1                    //go:build !anthropic
kanz/services/risk-engine/cmd/risk-engine/dedup_default.go:1         //go:build !redis
kanz/services/risk-engine/cmd/risk-engine/dedup_redis.go:1           //go:build redis
kanz/services/webhook-ingest/cmd/webhook-ingest/nonces_default.go:1  //go:build !redis
kanz/services/webhook-ingest/cmd/webhook-ingest/nonces_redis.go:1    //go:build redis
```

`coder/websocket` is imported **unconditionally** by six non-test files: `internal/exchange/netdial/netdial.go`, `internal/marketedge/depth/{binance,okx}.go`, `internal/marketedge/trades/{binance,okx}.go`, `services/venue-binance/internal/binance/binance_userdata.go`, `services/venue-okx/internal/okx/okx_userdata.go`.

`go list -deps` per binary, default build (tree@9021ec8):

| Binary | `coder/websocket` packages linked |
|---|---|
| `./cmd/kanz` | 0 |
| `./cmd/universe` | 0 |
| `./services/oms/cmd/oms` | 0 |
| `./services/venue-binance/cmd/venue-binance` | **4** |
| `./services/venue-okx/cmd/venue-okx` | **4** |
| `./services/market-ingest/cmd/market-ingest` | **4** |

The *substance* the claim protects is partly intact — the client and OMS binaries are websocket-free — but the **mechanism named in `KANZ_BRAIN.md` is absent**, and the venue/ingest binaries link the vendor unconditionally with no tag to strip it. By this repository's own rule (`KANZ_BRAIN.md:166`: *"a build tag that CI does not compile is not a feature — it is dead code with a plan attached"*), the inverse applies here: a build tag that documentation claims but the code lacks is a false architectural guarantee.

**Rate limiting / reconnect / backoff** *(tree@9021ec8)*: under the two adapter trees, 10 files match `rate.?limit`, 4 match reconnect, 2 match backoff.

**Instruments actually configured** — `git grep "INSTRUMENTS, value" origin/main`:

```
kanz/infra/deploy/market-ingest-deploy.yaml:106  MARKET_INGEST_INSTRUMENTS      = "BTC-USD,ETH-USD"
kanz/infra/deploy/market-ingest-deploy.yaml:112  MARKET_INGEST_BINANCE_SYMBOLS  = "BTC-USD=BTCUSDT,ETH-USD=ETHUSDT"
kanz/infra/deploy/market-ingest-deploy.yaml:113  MARKET_INGEST_OKX_SYMBOLS      = "BTC-USD=BTC-USDT,ETH-USD=ETH-USDT"
kanz/infra/deploy/venue-binance-deploy.yaml:150  BINANCE_SYMBOLS                = "BTC-USD=BTCUSDT,ETH-USD=ETHUSDT"
kanz/infra/deploy/venue-okx-deploy.yaml:155      OKX_SYMBOLS                    = "BTC-USD=BTC-USDT,ETH-USD=ETH-USDT"
```

Symbols are **configuration, not code** (`market-ingest/internal/config/config.go:69,73` parse `instrument_id -> venue symbol` maps), so adding an instrument is a manifest edit. **`OKB` appears nowhere**: `git grep -n "OKB" origin/main -- 'kanz/**' 'kanz-schemas/**'` returns zero lines.

---

## 8. Authentication & Authorization

**Two identity planes that deliberately do not meet** (`KANZ_BRAIN.md:155`): the HTTP `/v1` plane is **OIDC/JWT at the api-gateway** (Eighred SSO, with a dev HS256 fallback); the in-cluster operator plane is **SPIFFE/SVID**. The recorded decision states an OIDC principal, however privileged, cannot reach an operator-plane RPC, and an operator credential grants no `Trade`.

**The gateway refuses to start** without authentication *and* authorization configured — `kanz/services/api-gateway/internal/config/config.go:149-158`, commented *"enforced here the same way — by refusing to run, not by logging a warning that scrolls past."*

**Authorization is two layers.** `kanz/pkg/auth/authz.go` is a policy-bundle RBAC (`Action` strings such as `risk.read`, `risk.scenario`) plus **ABAC structural invariants enforced in code, not policy data** — tenant isolation and portfolio scope (`ClaimPortfolios`, `ResourcePortfolio`). At the edge, `services/api-gateway/internal/authz` layers a **capability model where the capability is a required parameter of route registration**, so a route without one does not compile (`authz/arch_test.go:126-136`).

**Three capabilities, not two** — `services/api-gateway/internal/authz/authz.go:31,44`: `Read`, `Trade`, and **`Operate`** (*"RUN THE PLATFORM. Provision, cordon, drain and relabel nodes; write the …"*).

### The full `/v1` surface: 27 routes

**16 declared in the golden table** (`internal/authz/arch_test.go:60-84`):

| Capability | Routes |
|---|---|
| `Read` | `GET /v1/portfolios/{id}/exposure`, `GET /v1/portfolios/{id}/measures`, `POST /v1/portfolios/{id}/scenario`, `GET /v1/health`, `GET /v1/households/{id}`, `GET /v1/securities/{id}`, `GET /v1/prices/{id}`, `GET /v1/exceptions`, `POST /v1/ask`, and 5 TradingView Broker API reads under `/v1/broker/*` |
| `Trade` | `POST /v1/orders`, `POST /v1/orders/{id}/cancel` |

**11 more in `internal/control/control.go:78-88`, all `Operate`:** `GET/POST /v1/control/nodes`, `GET /v1/control/clusters`, `GET /v1/control/provisions`, `POST /v1/control/test-connection`, `POST /v1/control/nodes/{name}/{cordon,uncordon,drain,region}`, `GET /v1/control/venues`, `PUT /v1/control/venues/{venue}/keys`.

### FINDING 8.1 — the golden route table does not cover the `control` package

`TestTheWholeRouteTableIsDeclared` states its own contract (`arch_test.go:47-52`): *"Every `/v1` route the gateway serves, and the capability it demands. A **NEW route — anywhere, in any handler package** — fails this test until someone writes it down here."* It registers exactly three handler packages (`arch_test.go:55-57`):

```go
gateway.New(nil).Routes(m)
orders.New(nil).Routes(m)
proxy.New(nil).Routes(m)
```

`control` is not among them, so **11 routes — including `POST /v1/control/nodes` (provisions a node) and `PUT /v1/control/venues/{venue}/keys` (writes exchange API keys) — are absent from the one place the repository says the whole surface is written down.**

**Calibrated honestly: this is an inventory gap, not an authorization hole.** `control_test.go:251` (`TestEveryControlRouteDemandsOperate`) applies the same property test package-locally, and `TestControlRoutesRefuseReadAndTradeRoles` (`:85`) proves a read or trade token is rejected. Capability correctness **is** guarded. What is not guarded is the claim that the golden table is exhaustive — and that claim is what a future reviewer will rely on.

**Hierarchy** (`KANZ_BRAIN.md:152`, decided 2026-07-16, SOV-01 closed): the target blueprint's `root@universe` → `root@company` → `user_rw`/`user_ro` tree is considered satisfied by different machinery — the **`tenant` claim is the hierarchy** (binding sub-user → company → `app.tenant_id` → RLS), and **`Read`/`Trade` is the RO/RW split**. A `root@company` tier holding privileges a `user_rw` lacks was **deliberately not built**; the accepted consequence, stated in the entry, is that *"a tenant master and a trader are indistinguishable on `/v1`."* The `Operate` capability added later is the operator tier and is orthogonal to the tenant tree.

---

## 9. Secret Management

**Design:** memory-only. Vault CSI mounts secrets as files; services read them through a local `secret()` helper preferring `<KEY>_FILE` over a plaintext env var.

**No product path writes a secret to disk** *(tree@9021ec8)*: searching `os.WriteFile`/`ioutil.WriteFile` near key/secret identifiers across `kanz/` yields two hits, both in tests (`cmd/kanz-provisioner/join_test.go:68`, `cmd/universe/model_validation_test.go:275`). No `.env` files are tracked.

**`SetVenueKeys` is write-only by construction** — the `secrets.Store` interface exposes no value-returning method; `ListVenueKeys` is presence-only. `KANZ_BRAIN.md:165` records that key writes are **validate → prove → write**: the operator proves a key against the exchange before writing, a failed proof performs no write, and the exchange's response body never reaches the client.

**One credential *is* persisted to disk, by design:** the `kanz` CLI's SSO bearer token at `{os.UserConfigDir}/kanz/token.json`, mode 0600 (`cmd/kanz/internal/tokenstore/tokenstore.go`). That is the session-persistence mechanism, not an exchange key — but it is the one on-disk secret in the product, and belongs next to any zero-disk-footprint goal.

### FINDING 9.1 — SEC-CONFIG-001 is open at 17/17, and the board says otherwise

`git grep -l "func secret(" origin/main -- 'kanz/**/*.go'` → **17 files** (16 services + `cmd/kanz-migrate/main.go`). I inspected the body of every one: **all 17 are identical in the relevant lines and all 17 discard the read error.**

```go
func secret(k string) string {
    if p := os.Getenv(k + "_FILE"); p != "" {
        if b, err := os.ReadFile(p); err == nil {   // <- error dropped
            return strings.TrimSpace(string(b))
        }
    }
    return os.Getenv(k)                              // <- silent fallback
}
```

A mounted-but-unreadable Vault CSI secret is therefore indistinguishable from one never configured, and the process falls back to the plaintext env var.

**The board's status line is wrong for `main`.** `KANZ_TASKS.md:165` (on branch `docs/audit-followups`) reads *"`venue-binance` and `venue-okx` are fixed; the other 15 are not."* At the pinned rev they are **not** fixed — `venue-binance/internal/config/config.go:107-114` still carries the `err == nil` form, identical to `oms/internal/config/config.go:195-202`. The fix exists only on the **unmerged** branch `fix/venue-orderview-durability` (`a996c18`, *"fix(venue): fail fast on unreadable declared secret mount"*). The claim is true of that branch and false of `main`.

**The calibration recorded on the board holds and is worth preserving:** the two highest-risk fail-open shapes are safe — the api-gateway refuses to start when both `OIDCIssuer` and `JWTSecret` are empty (`config.go:149-151`), and `OIDCIssuer` is read via plain `os.Getenv` (`:113`), **not** `secret()`, so an unreadable file cannot silently downgrade production OIDC to the dev HS256 path. This is a durability and diagnosability defect, not a currently exploitable one.

---

## 10. Testing

- **441** `*_test.go` files; **38** architecture guards in `kanz/test/arch/`; tiers `arch`, `contract`, `e2e`, `load`, `mtls`.
- **24** pytest files in `kanz-py/tests/`.
- `go build ./...` → exit 0 (verified). The full-suite result quoted by the prior inventory (175 packages ok, 0 FAIL) is **board/prior-session sourced**; I did not re-run the suite.

**Known coverage hazards** (board-sourced; each already cost this repo a real bug):
- Postgres-gated tests **skip silently** without `TEST_POSTGRES_URL`, and must run as a **NOSUPERUSER** role or RLS is bypassed and the isolation tests pass falsely.
- `go test ./...` races on a shared `TEST_POSTGRES_URL`; `-p 1` is required.
- `fakeBus` (the OMS test double) does **not** validate envelopes, so it accepts what a real broker rejects — this let a Critical tenant-context bug ship past a green suite.

**Guard-scope hazard found in this pass:** FINDING 8.1 — a guard whose stated scope exceeds its actual scope is a third instance of the same failure mode.

---

## 11. Configuration

Per-service `internal/config/config.go` with a `Load() (Config, error)` constructor (e.g. `services/oms/internal/config/config.go:154`). Values come from environment variables; `<KEY>_FILE` takes precedence for secrets (§9).

**There is no environment/profile selector** — no `KANZ_ENV`, no `ENVIRONMENT` marker read by any binary *(tree@9021ec8; searched `services/venue-binance/internal/config/config.go` and `infra/deploy/venue-binance-deploy.yaml`)*. Environment separation is expressed by **which values are injected** (manifest + Vault path), not by a mode flag the binary reads.

**Simulators are never reachable by omission** (`KANZ_BRAIN.md:183`): every simulated source (`SimVenue`, `SimFeed`, `StubModel`, sim depth) requires an explicit affirmative flag (`DATAMASTER_ALLOW_SIM`, `COPILOT_ALLOW_STUB`), and the process refuses to start otherwise. The entry notes this has been reachable-by-accident four separate times.

---

## What this repository can actually do today — 10 honest points

1. **Run the full automated trading loop end-to-end**, cluster-proven per the board: signed TradingView webhook → `StrategySignal` FACT → venue-allocated `SubmitOrder` → OMS gate → venue → fills → bitemporal projection.
2. **Survive a crash mid-order without double-trading** — the OMS reconciles interrupted orders against venue truth and re-drives or quarantines by policy, with idempotency anchored on `order_id == clOrdId`.
3. **Refuse to trade what it cannot value** — the pre-trade gate terminally rejects unpriced MARKET/STOP orders instead of valuing them at zero.
4. **Enforce tenant isolation at the database** with `FORCE` RLS, asserted by an arch guard and seven dedicated migrations.
5. **Do exact decimal money arithmetic** with `float64` banned on every capital path and enforced by two arch guards.
6. **Publish a signed, scanned, SBOM-attested 26-image release** (`v0.2.0`), with a `pin-digests` job that rewrites manifests to immutable digests.
7. **Operate the Kubernetes estate from a TUI** — `universe` joins, cordons, drains, relabels and writes venue keys through 11 `Operate`-gated `/v1/control/*` routes, holding no cluster credential of its own.
8. **Halt trading platform-wide** via `kanz-halt`'s broadcast `ModeChanged` FACT — deny-by-default, fail-closed and **latching**: reconnecting the bus does not clear it and no automated actor can; only an operator resume reopens the gate.
9. **Serve TradingView natively** — `services/tv-sync/internal/brokerapi/` implements the TradingView **Broker API**, exposed as 5 `Read` routes under `/v1/broker/*`. The target system's §1.4 "is this feasible?" question is already answered in code, by a chosen path.
10. **What it cannot do.** It has **no fund-basket concept whatsoever** (§5 — four comments, zero schema); its `kanz` terminal is a **544-LOC, 8-command line REPL**, not a multi-screen TUI, and cannot manage keys, baskets or nodes; **`OKB` is absent from the entire repository** and only BTC-USD/ETH-USD are configured; it has **never executed a disaster-recovery failover**; it has **no running Prometheus**, so every degraded mode is unobservable in production; and it **cannot say which database cluster the OMS order store is backed up in**.

---

## Corrections to the 2026-07-27 inventory

This document supersedes the prior `docs/ANALYSIS.md`. Three of its claims are wrong at `4ca6a9a`:

| Prior claim | Correction |
|---|---|
| *"Where are the venue build tags? …I did not locate the tagged files"* (Open Question 3) | **They do not exist.** No `//go:build binance\|okx` anywhere in the module; `coder/websocket` is linked unconditionally by the venue and ingest binaries (FINDING 7.1). |
| *"`Read`/`Trade` is the RO/RW split"* — two capabilities, 16 routes | **Three capabilities, 27 routes.** `Operate` gates 11 `/v1/control/*` routes (§8). |
| *"`secret()` … fixed in `venue-binance`/`venue-okx`"* (2 of 17) | **0 of 17 are fixed on `main`.** The fix lives only on the unmerged branch `fix/venue-orderview-durability` (FINDING 9.1). |

---

## OPEN QUESTIONS

1. **Which database does each service actually use in production?** (`ONBOARD-M6`) — the repo gives conflicting answers; the truth is a `vault kv get` not runnable from here.
2. **Are `oms`, `tv-sync`, `venue-*`, `regulatory` in any DR cluster?** They hold `*-db` SecretProviderClasses but appear in no cluster under `kanz/infra/dr/postgres/` and in no written exclusion.
3. **Is there a production environment at all?** The only kubectl context found in prior sessions was a kind rig; `kanz-data`, `spire-system` and `vault` namespaces were absent.
4. **Was the `//go:build binance|okx` split ever built and later removed, or only ever written down?** `git log -S"go:build binance"` would settle it. It is a question about intent, and the answer decides whether FINDING 7.1 is drift or aspiration.
5. **What lands, and in what order?** `origin/main` advanced and local `HEAD` changed branches *during* this analysis. Six unmerged local branches carry work — `fix/venue-orderview-durability`, `fix/provisioning-job-ttl`, `fix/rig-digest-rewrite`, `docs/audit-followups`, `docs/demote-release-readiness`, `chore/docs-dedupe` — and at least one holds a fix this document reports as missing from `main`. Any plan must first decide what merges.

---

**Phase 0 ends here.** No gap analysis, no plan, no code — per the working rules, those await explicit approval.
