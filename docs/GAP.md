# Gap Analysis — Phase 2

**Pinned to:** `origin/main` @ `4ca6a9a`. **Date:** 2026-07-28.
**Input:** `docs/ANALYSIS.md` (Phase 0 inventory) vs. the target system (Phase 1).
**Rule applied:** where the repository has a *recorded decision* rejecting a target requirement, the gap is marked **DECIDED-AGAINST** rather than MISSING. That distinction matters: a missing thing is work; a decided-against thing is a product decision to re-open, and re-opening it costs more than building it because something was built in its place.

Status: `EXISTS` / `PARTIAL` / `MISSING` / `DECIDED-AGAINST` / `FLAWED`
Effort: `S` ≤1 day · `M` 1–3 days · `L` 1–2 weeks · `XL` >2 weeks
Priority: `P0` MVP blocker · `P1` · `P2`

---

## 1.1 Entry & TUI

| # | Requirement | Status | Evidence | Missing | Effort | Pri |
|---|---|---|---|---|---|---|
| 1 | Typing `kanz` in any shell launches the TUI | **MISSING** | `kanz/Makefile:57-59` — `release` builds `-o bin/ ./services/...` **only**. No `Dockerfile` under `cmd/kanz/`; `ls kanz/cmd/*/Dockerfile` → `kanz-halt`, `kanz-migrate`, `kanz-provisioner`. `release.yml` ships 26 *service* images and no client artifact. | Any distribution path at all: no `install` target, no cross-compiled artifacts, no release attachment, no installer. Today the only way to run it is `go run ./cmd/kanz`. | **M** | **P0** |
| 2 | First run does cryptographic device registration via SSH key fingerprint | **DECIDED-AGAINST** | `KANZ_BRAIN.md:155` — SSH-fingerprint identity was on the table and rejected: *"an HTTP edge has no SSH handshake to carry a fingerprint in"*. Delivered instead: OIDC device flow (`cmd/kanz/main.go`, `pkg/deviceauth`). | Nothing, unless the decision is re-opened. The device flow **is** a first-run registration ceremony; it is not key-based. | — | P2 |
| 3 | Session state remembered securely, no repeated credential prompts | **EXISTS** | `cmd/kanz/internal/tokenstore/tokenstore.go` — `{os.UserConfigDir}/kanz/token.json`, 0600, atomic write-then-rename; `Valid()` refreshes with 30 s skew. | — | — | — |
| 4 | Claude-Code-like single-window TUI with keyboard navigation and sub-screens | **PARTIAL** | `cmd/kanz` is a **544-LOC line REPL** over `os.Stdin`/`os.Stdout` with 8 slash commands (`repl.go:98-121`); it does not import bubbletea. The framework *is* in the repo and proven — `cmd/universe` and `cmd/kanz-monitor` are full bubbletea TUIs. | Full-screen rendering, keyboard navigation, sub-screens for `cmd/kanz`. **Reuse, not new work:** `universe`'s `model.go`/`view.go`/`form.go` are the pattern to extend. | **L** | **P1** |

## 1.2 In-TUI capabilities

| # | Requirement | Status | Evidence | Missing | Effort | Pri |
|---|---|---|---|---|---|---|
| 5 | Basket CRUD: name, allocated capital, allocation % | **MISSING** | `git grep -in basket` → **4 hits, all comments** (`internal/execution/router.go:63`, `services/oms/internal/order/service.go:69`, `internal/execution/account_test.go:67-68`). `risk-engine/migrations/0001_state.sql:15` `portfolios` is an **engine snapshot** — no allocated-capital column, no weights, no user write path. | The entire concept: schema, proto, service ownership, `/v1` routes, TUI screens. **And a prior decision blocks the obvious design** — `KANZ_BRAIN.md:152` states fund-basket allocation is *deploy-time* by SOV-02's reasoning, and that an API mutating the binding at runtime re-opens a refusal invariant. | **XL** | **P0** |
| 6 | API key management; keys never on disk, RAM only | **EXISTS** (not in `kanz`) | `PUT /v1/control/venues/{venue}/keys` (`control.go:88`, `Operate`); write-only `secrets.Store` with no value-returning method; validate→prove→write (`KANZ_BRAIN.md:165`); Vault CSI mounts (`venue-binance-deploy.yaml:177-188`) with `readOnlyRootFilesystem: true`. Driven from the **`universe`** TUI (`keyform.go`), not `kanz`. | Only the placement: the capability exists and is well-built, on the operator TUI. Nothing to build unless it must move. | S (if surfaced in `kanz`) | P2 |
| 7 | Deploy **and remove** nodes from the TUI | **PARTIAL** | Deploy/lifecycle exists: 11 `Operate` routes (`control.go:78-88`) and 11 matching RPCs (`operator/v1/operator.proto:27-70`) — `AddNode`, `Cordon`, `Uncordon`, `Drain`, `SetNodeRegion`, `TestConnection`. | **No removal.** There is no `RemoveNode`/`DeleteNode`/`Wipe` RPC in the 11. Drain + cordon evacuate a node; nothing deletes it from the estate. `KANZ_BRAIN.md:154` defines the k8s equivalent of revocation (stop workloads, purge Vault-CSI secrets, scale tenant to zero) but no RPC performs it. | **M** | **P1** |
| 8 | Real-time positions, PnL, order book, risk in the TUI | **PARTIAL** | `cmd/kanz-monitor` is a **separate** bubbletea TUI with a bus reader and poller (`busreader.go`, `poller.go`, `model.go`). `cmd/kanz` exposes only `/exposure`, `/measures`, `/scenario` — request/response, not streaming. | Consolidation into one terminal, and an order-book view. Three TUI binaries (`kanz`, `universe`, `kanz-monitor`) is the parallel-surface smell this repo forbids elsewhere. | **L** | **P1** |
| 9 | Kill-switch: remote halt **plus** wipe of binary payloads, RAM and DB connection state | **PARTIAL / DECIDED-AGAINST** | Halt exists and is strong: `cmd/kanz-halt/main.go:1-27` broadcasts `lifecycle.v1.ModeChanged`; the gate is **deny-by-default, fail-closed and latching** — reconnecting the bus does not clear it and no automated actor can. **Wipe does not exist and was cancelled**: `KANZ_BRAIN.md:154` — *"'Wipe a node' is not a host concept here"*; the `WipeNodeConfiguration` RPC belonged to the cancelled SSH deployment model. | The halt half is delivered. The wipe half is a rejected architecture, not a backlog item. | — | P2 |

## 1.3 Trading behaviour (MVP)

| # | Requirement | Status | Evidence | Missing | Effort | Pri |
|---|---|---|---|---|---|---|
| 10 | OKX + Binance only | **EXISTS** | `services/venue-binance/`, `services/venue-okx/`, each its own process and image; `OMS_VENUE_ENDPOINTS` selects adapters at runtime (`services/oms/Dockerfile:11-23`). | — | — | — |
| 11 | BTC, ETH, **OKB** | **PARTIAL** | Configured: `MARKET_INGEST_INSTRUMENTS = "BTC-USD,ETH-USD"` (`market-ingest-deploy.yaml:106`), plus symbol maps in 4 more manifests. **`git grep -n "OKB"` over `kanz/**` and `kanz-schemas/**` → zero lines.** | OKB. Symbols are configuration, not code (`market-ingest/internal/config/config.go:69,73`), so this is a manifest edit **plus** a reference-data/pricing check — OKB is OKX-only, so the Binance symbol map has no counterpart and the router must not be asked for one. | **S** | **P1** |
| 12 | Signal → Risk Gate → Order chain; signals never reach an exchange directly | **EXISTS** | `services/oms/cmd/oms/main.go:208` builds `comp.NewPreTradeGate(...)`, wrapped at `:230` by `compliance.NewCOMP01Gate(...)`. Engine at `internal/compliance/gate.go:21` — *"the COMP-01c pre-trade enforcement point"*. Phases declared at `internal/compliance/decisions.go:11`. Order routing is out-of-process to the venue adapters, so no signal path bypasses the OMS. | — | — | — |
| 13 | Strictly idempotent orders; retries never duplicate | **EXISTS** | `KANZ_BRAIN.md:18` — `order_id` **is** the venue `clOrdId`/`newClientOrderId`, so an ambiguous-timeout submit recovers by *querying that id*; `fill_id` deterministic (`{venueSymbol}-{tradeId}`). Mechanism centralised in `internal/execution/{venue,query,closes,grpc_venue,fixvenue}.go`, not duplicated per adapter. | — | — | — |

## 1.4 TradingView tracking

| # | Requirement | Status | Evidence | Missing | Effort | Pri |
|---|---|---|---|---|---|---|
| 14 | Lightweight TradingView overlay for open positions + health | **EXISTS — the feasibility question is already answered in code** | `services/tv-sync/internal/brokerapi/brokerapi.go` implements the TradingView **Broker API**; exposed as 5 `Read` routes: `GET /v1/broker/accounts`, `…/{id}/state`, `…/{id}/positions`, `…/{id}/orders`, `…/{id}/executions` (`proxy.go:110-114`). Backed by a bitemporal fact log (`tv-sync/migrations/0001_fact_log.sql`). | Nothing architectural. The Phase 1 brief asks to investigate Pine Script vs webhook direction; **that investigation is moot** — the repo chose the third and strongest option (TV as a broker-integration client of our API) and shipped it. What remains is operational: TradingView must whitelist the integration. | — | P2 |
| 15 | Strategy details must never leak; only position/PnL telemetry | **EXISTS** | The Broker API surface is reads of the fund's own book (accounts/state/positions/orders/executions) — no signal, model or rule is exposed. All 5 routes are `Read`; `arch_test.go:80-84` pins them. Release binaries are `-trimpath -ldflags="-s -w"` (`Makefile:48-59`). | — | — | — |

## 1.5 Authorization hierarchy

| # | Requirement | Status | Evidence | Missing | Effort | Pri |
|---|---|---|---|---|---|---|
| 16 | Global Sovereign (`root@universe`) — topology, global risk, system halt | **EXISTS** | The `Operate` capability (`authz.go:31,44`) gates 11 `/v1/control/*` routes; `kanz-halt` is the platform brake; the operator plane is SPIFFE-identified and **cannot be reached by an OIDC principal** (`KANZ_BRAIN.md:155`). | — | — | — |
| 17 | Tenant Admin (`root@company`) — user onboarding, basket definitions, key lifecycle | **DECIDED-AGAINST** | `KANZ_BRAIN.md:152` (SOV-01, closed 2026-07-16): a `root@company` tier holding privileges a `user_rw` lacks was **deliberately not built**; a `Manage` capability + tenant-admin API was explicitly rejected, because sub-user creation belongs to Eighred SSO, basket allocation is deploy-time, and keys are Vault mounts never reachable from a request path. **Accepted consequence, stated in the entry:** *"a tenant master and a trader are indistinguishable on `/v1`."* | The decision names its own re-open trigger: **tenant self-service**. Re-opening it requires re-opening SOV-02's deploy-time basket binding **first** — which is exactly requirement #5. | **L** (if re-opened) | P1 |
| 18 | Sub-user — Write (open/close positions, modify strategies) | **EXISTS** | `authz.Trade` gates `POST /v1/orders` and `POST /v1/orders/{id}/cancel`; `TestEveryRouteOnTheCapitalPathRequiresTRADE` (`arch_test.go:30-44`) enforces it as a *property* over whatever the orders package registers. | — | — | — |
| 19 | Sub-user — Read-only | **EXISTS** | `authz.Read`; `TestAReadCapabilityCannotBeGrantedTradeByAccident` (`arch_test.go:107`) proves the baseline role cannot smuggle in `Trade` and that no-role is deny-by-default. | — | — | — |

## 1.6 Long-term architecture — compatibility check only (not MVP work)

| Target | Compatible? | Evidence |
|---|---|---|
| Multi-source data (TV, macro, alternative) | **Yes** | `services/alternatives`, `services/datamaster`, `internal/marketedge`; the bus envelope carries lineage and quality flags. |
| Knowledge graph | **Unblocked, unbuilt** | Nothing conflicts; `services/lineage` is the nearest existing seam. |
| AI prediction engine (time-series + GNN + LLM) | **Yes — partly built** | `kanz-py/kanz_inference/` (22 modules) + `inference-deploy.yaml`; `internal/prediction` now has 3 non-test importers. |
| XAI (rationale, confidence, analogies) | **Yes — partly built** | `kanz_inference/explain/explainer.py` (permutation-Shapley), `governance/model_card.py`, `governance/promotion.py`. |
| Monte-Carlo stress (VaR/CVaR/ES) | **Yes** | `internal/risk` is 12,109 LOC and already carries the scenario surface (`POST /v1/portfolios/{id}/scenario`). |
| Basket isolation (no cross-basket margin) | **Designed for, not enforceable yet** | The invariant is written down (`internal/execution/router.go:63`, `oms/internal/order/service.go:69`) and per-venue credential separation implements part of it — but with **no basket entity** (#5) there is nothing to isolate. |
| Auto-deleveraging | **Unblocked, unbuilt** | The pre-trade gate is the natural host; no auto-unwind actor exists. |
| Financial precision, no float on money | **Yes — delivered** | `internal/dec/dec.go:4`; two arch guards (`decimal_domain_test.go`, `decimal_conversion_test.go`). |
| Bitemporal ledger | **Yes — delivered** | `accounting/0001_ledger.sql` (`effective_time` + `knowledge_time`), WORM trigger in `0004_ledger_worm.sql`. |
| Zero-disk secret footprint | **Yes, with one exception** | Vault CSI + `readOnlyRootFilesystem: true`; no product path writes a secret to disk. **Exception:** the `kanz` CLI persists its SSO bearer token at `token.json` 0600 — by design, and required by requirement #3. |

---

## A. Critical defects — monetary or security exposure

**A1 · `secret()` swallows the read error in all 17 composition roots — `FLAWED` · Effort M · P0**
`git grep -l "func secret(" origin/main` → 17 files; every one carries `if b, err := os.ReadFile(p); err == nil` and falls through to the plaintext env var. A mounted-but-unreadable Vault CSI secret is indistinguishable from one never configured.
*Calibrated:* **not currently exploitable.** The api-gateway refuses to start when both `OIDCIssuer` and `JWTSecret` are empty (`config.go:149-151`), and `OIDCIssuer` is read by plain `os.Getenv` (`:113`), **not** `secret()` — so an unreadable file cannot silently downgrade production OIDC to the dev HS256 path. This is durability and diagnosability.
*Board is wrong:* `KANZ_TASKS.md:165` says venue-binance/okx are fixed. They are not, on `main`. The fix exists only on unmerged `fix/venue-orderview-durability` (`a996c18`).
*The deliverable is the guard,* not the 17 edits: one shared helper that distinguishes declared-but-unreadable from absent, plus an arch guard forbidding a new local `secret()`. Seventeen copies of one helper is what `internal/dec` was consolidated to eliminate.

**A2 · The CI build matrix tells operators the OMS image is a simulator. It is not. — `FLAWED` · Effort S · P0**
`.github/workflows/build.yml:52-56` states the `oms` image is *"the vendor-free build — no Binance, no OKX, SimVenue only… It is a SIMULATOR. A trading image is an explicit `--build-arg GOTAGS=binance,okx` build and must not share this tag."*
`kanz/services/oms/Dockerfile:11-23` says the exact opposite, and is correct: *"There is exactly one OMS image… After INFRA-M7a there are no venue build tags at all… it is the SAME image whether it trades Binance, OKX, both, or nothing."*
**Proven:** `go list -deps` vs `go list -deps -tags "binance okx"` on `./services/venue-binance/cmd/venue-binance` → **560 packages, byte-identical**. The tags are inert.
*Why this is P0 despite being a comment:* it is a deployment instruction. An operator following it believes the shipped OMS cannot trade and that a second image must be built to go live. Both beliefs are false. The one thing this repo has been burned by four times is a simulator reachable by omission (`KANZ_BRAIN.md:183`) — here the confusion runs the other way and is equally dangerous.

**A3 · The golden route table's stated scope exceeds its actual scope — `FLAWED` · Effort S · P1**
`arch_test.go:47-52` promises *"a NEW route — anywhere, in any handler package — fails this test until someone writes it down here."* It registers `gateway`, `orders`, `proxy` (`:55-57`) and **not `control`**, so 11 `Operate` routes — including `POST /v1/control/nodes` and `PUT /v1/control/venues/{venue}/keys` — are absent from the table.
*Calibrated:* **not an authorization hole.** `control_test.go:251` applies the same property test package-locally, and `:85` proves read/trade tokens are refused. What is broken is the exhaustiveness claim a future reviewer will rely on.

**A4 · Two Prometheus rule files alert on metrics nothing emits — `FLAWED` · Effort S · P2**
`infra/observability/alerts/data-quality.rules.yaml` (8 metrics) and `slo/slo.recording.rules.yaml` (`kanz_data_staleness_lag_seconds`) reference metrics from the deleted `internal/integrity` (confirmed absent). Dead alerts are worse than no alerts: they read as coverage.

---

## B. Architectural debt

**B1 · Three terminal binaries, one user — `refactor` · L**
`kanz` (REPL client), `universe` (operator TUI), `kanz-monitor` (monitoring TUI). Requirements #4 and #8 both resolve to *one* terminal. Building basket screens into `kanz` without first deciding this creates a fourth surface. **Keep `universe`** — it is the only one at the required fidelity and its `model/view/form/poller` split is the pattern to reuse; fold `kanz-monitor` into it or into `kanz`. Decide before #4 starts, not after.

**B2 · A retired build-tag architecture is still documented as live in four places — `delete` · S**
`Makefile:18-19` (`VENUE_TAGS := binance okx`), `:57` (`TAGS` in the release doc-comment), `:73,77` (`test-venues`, `build-venues`), `KANZ_BRAIN.md:17,44`, `build.yml:52-56` (A2), and vestigial `-tags "${GOTAGS}"` in `oms`/`archiver`/`lake-sink` Dockerfiles. The tags were removed **on purpose** when venues became out-of-process (`oms/Dockerfile:17`) — that decision is right and stands. The residue is inert scaffolding that reads as a live safety control. Deleting it is strictly subtractive.

**B3 · Board and code disagree about what is fixed — `process` · S**
Six unmerged local branches carry work, at least one of which (`fix/venue-orderview-durability`) holds a fix the board reports as landed (A1). Any plan that starts before deciding what merges will re-implement or re-verify work that already exists.

**B4 · No environment/profile selector — `keep, with eyes open` · —**
No `KANZ_ENV`/`ENVIRONMENT` is read by any binary; separation is by injected values (manifest + Vault path). This is a deliberate, defensible posture — a binary that cannot know it is "in dev" cannot take a dev shortcut. It stays as is; it is listed so nobody "fixes" it.

---

## C. Over-engineering — non-MVP weight

Non-test LOC not named by any MVP requirement:

| Package | LOC | Verdict |
|---|---|---|
| `internal/regulatory` (FRTB/DRC) | 1,359 | **Freeze.** Institutional analytics with no MVP consumer. |
| `internal/optimization` | 1,343 | **Freeze.** |
| `internal/collateral` (SIMM, financing) | 548 | **Freeze.** |
| `internal/alternatives` + `services/alternatives` | 683 | **Freeze.** No automated producer — only the `kanz-altevent` CLI publishes. |
| `internal/wealth` + `services/wealth` | 463 | **Freeze.** Same: only `kanz-household` publishes. |
| `services/web-bff` | — | **Already parked** (PS-01…07 cancelled 2026-07-13) — but still built and imaged. |

**Freeze, do not delete.** Total ≈3,250 LOC of analytics plus two under-driven services. They compile, they are tested, they carry arch guards, and deletion is a large irreversible change with no MVP payoff — that fails the cost check. What they *should* stop doing is consuming attention: no new work, no new tasks, no roadmap lines until an MVP requirement names them.

**One genuine removal candidate:** `services/web-bff`. The product surface it serves was cancelled 15 days ago, it is still in the 26-image build/release matrix, and every release signs, scans and pins it. That is recurring cost for a decided-dead surface. **Recommend: remove from the image matrix, keep the source.**

---

## OPEN QUESTIONS carried into Phase 3

1. **What merges, and in what order?** (B3) — six unmerged branches; `fix/venue-orderview-durability` overlaps A1 directly.
2. **Is basket allocation runtime or deploy-time?** Requirement #5 needs the answer, and `KANZ_BRAIN.md:152` says deploy-time *by SOV-02's own reasoning* — *"an account bound to two portfolios must refuse to START, which an API that mutates the binding at runtime re-opens."* Requirement #17 depends on the same answer. **This is the single highest-leverage unknown in the whole gap set** and it is a product decision, not a technical one.
3. **Does OKB need a Binance counterpart?** OKB is OKX-listed; requirement #11 assumes a symbol map per venue.
4. Production DB topology, DR cluster membership, and whether a production environment exists — carried unchanged from `docs/ANALYSIS.md`.

---

**Phase 2 ends here.** `docs/PLAN.md` follows.
