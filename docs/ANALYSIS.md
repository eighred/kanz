# Repository Analysis — Phase 0

**Date:** 2026-07-27 · **Method:** direct inspection. Every claim carries a `file:line` or a command whose output is quoted. Where I could not find something, I name the paths I searched rather than asserting absence.

**Scope note.** This is Phase 0 (inventory) only. No gap analysis, no plan, no code.

---

## 1. Language / Runtime / Build System

| Component | Location | Notes |
|---|---|---|
| **Go module** | `kanz/go.mod` | `module github.com/kanz-eng/kanz`, `go 1.26.1`, `toolchain go1.26.5`. Single module rooted at `kanz/`. |
| **Go build** | `kanz/Makefile` | Present. `release` target does `-trimpath -ldflags="-s -w"` per `KANZ_BRAIN.md`. |
| **Python** | `kanz-py/pyproject.toml` | The `inference` service. `kanz-py/Dockerfile` → `ENTRYPOINT ["python","-m","kanz_inference"]`. |
| **TypeScript** | `kanz/ts/kanz-bus/package.json` | Cross-language bus client. Node pinned via `.nvmrc` = 24. |
| **Protobuf** | `kanz-schemas/buf.yaml` | buf-managed. Generated code is **not committed** (gitignored); a CI guard rejects tracked generated files. |
| **CI** | `.github/workflows/` | `kanz-ci`, `kanz-build` (26-image matrix), `release` (26-image matrix), `security`, `latency`, `preview`, `CodeQL Advanced`, `Dependency Graph`. |

**Load-bearing dev seam:** `kanz/go.mod` carries `replace github.com/kanz-eng/kanz-schemas-go => ../kanz-schemas/gen/go`. Every Dockerfile therefore builds from the **repo root**, not `kanz/`, and COPYs `kanz-schemas/gen/go/` in — see `kanz/cmd/kanz-halt/Dockerfile:5-8`.

---

## 2. Directory Map

| Path | Purpose |
|---|---|
| `kanz/` | The Go module: all services, shared packages, infra manifests, tests. |
| `kanz/cmd/` | 12 standalone binaries (see §3). |
| `kanz/services/` | 26 service trees, each `package main` under `cmd/<name>/`. |
| `kanz/internal/` | Shared internal packages promoted on a second consumer (`dec`, `execution`, `venueadapter`, `risk`, `signal`, `marketedge`, …). |
| `kanz/pkg/` | Public-boundary packages (`bus`, `auth`, `transport`, `alpha`). |
| `kanz/infra/` | Kubernetes manifests, NATS/Kafka topology, DR, security, observability, GitOps. |
| `kanz/test/` | `arch/` (38 guard files), `contract/`, `e2e/`, `load/`, `mtls/`. |
| `kanz-py/` | Python inference service. |
| `kanz-schemas/` | Protobuf contracts + buf config. |
| `tools/` | Repo-root operational scripts: `rig-apply.sh`, `rig-images.sh`, `rig_dev_patch.py`, `validate-board.sh`. |
| `docs/` | `RELEASE_READINESS.md` (now a dated audit), `superpowers/{specs,plans}`, `runbooks/`. |

---

## 3. Entry Points

**12 binaries under `kanz/cmd/`:**

`kanz` · `universe` · `kanz-monitor` · `kanz-halt` · `kanz-migrate` · `kanz-provisioner` · `kanz-altevent` · `kanz-household` · `kanz-mandate` · `kanz-devtoken` · `kanz-pin-digests` · `kanz-tenantgen`

**The two terminal clients are different products, and this matters:**

- **`kanz`** (`kanz/cmd/kanz/main.go:1-6`) — *"the Eighred institutional risk & portfolio terminal client: a Claude-Code-style REPL over the delivered api-gateway /v1 edge. It signs in via the Eighred SSO **device flow** (`pkg/deviceauth`), persists the token, and drives natural-language turns through the copilot and slash commands through the risk endpoints. It is a pure client — no backend, no mocks."* Internals: `config`, `gateway`, `repl`, `tokenstore`.
- **`universe`** (`kanz/cmd/universe/main.go:1-8`) — *"the **operator TUI**: it lists the Kubernetes estate (nodes, clusters), provisions new nodes, drives node lifecycle (cordon/drain/region) and manages venue API credentials… IT REACHES THE ESTATE THROUGH THE API GATEWAY, and holds no cluster access of any kind (OPS-M2c)."*

**26 services** under `kanz/services/`: accounting, alternatives, api-gateway, archiver, audit, autopilot, compliance, copilot, datamaster, lake-sink, lineage, market-data, market-ingest, oms, operator, optimization, performance, regulatory, risk-engine, schema-registry, tv-sync, venue-binance, venue-okx, wealth, web-bff, webhook-ingest.

---

## 4. Existing Components — implemented vs. skeleton

**Verified live and exercised** (cluster-proven per `KANZ_TASKS.md` readiness rows, each with commit evidence): the trading loop end-to-end (webhook-ingest → signal → OMS → SimVenue → fills → tv-sync projection), the OMS crash-recovery sweep with `venue_ack_at` / `cancel_announced_at` / `outcome_announced_at` markers, the compliance pre-trade gate, the archiver (NATS→Kafka single writer), the operator TUI's node lifecycle (join, cordon, uncordon, drain, region relabel, venue-key write), and NATS mTLS with SPIFFE SVID→user mapping.

**Built but not wired to an automated producer:** `alternatives` and `wealth` — their only publishers are the `kanz-altevent` / `kanz-household` operator CLIs (`KANZ_TASKS.md`, alternatives/wealth row).

**Parked by decision:** `web-bff` — the end-user product surface (PS-01…PS-07) was cancelled 2026-07-13; the service stays built.

**Deleted, with the reasoning recorded:** `internal/integrity` (DATA-M4). I searched `kanz/internal/integrity` — the directory does not exist.

**Dead references I found (not dead code — dead *pointers*):**
- `kanz/infra/observability/alerts/data-quality.rules.yaml` — all 8 metrics it alerts on are absent from the Go code; they were emitted by the deleted `internal/integrity`.
- `kanz/infra/observability/slo/slo.recording.rules.yaml` — consumes `kanz_data_staleness_lag_seconds`, same orphan.

---

## 5. Data Layer

**13 services own SQL migrations:** accounting (4), alternatives (2), audit (1), datamaster (3), market-data (1), oms (4), regulatory (1), risk-engine (3), schema-registry (1), tv-sync (1), venue-binance (2), venue-okx (2), wealth (2).

**Accounting ledger** — `kanz/services/accounting/migrations/0001_ledger.sql` defines `ledger_entries` and `ledger_snapshots`. **Bitemporal**: `effective_time` + `knowledge_time` columns confirmed present in `0001_ledger.sql`. **WORM**: `0004_ledger_worm.sql` adds an engine-level append-only trigger (added after an audit found the ledger rewritable while `audit_log` already had one).

**Other bitemporal stores:** `accounting/0003_venue_account_scope.sql`, `market-data/0001_price_history.sql`.

**Tenancy:** Postgres RLS with `ENABLE` + `FORCE ROW LEVEL SECURITY` and an `app.tenant_id` GUC, deny-by-default; asserted by `kanz/test/arch/tenant_scope_test.go`.

**Precision:** `kanz/internal/dec/dec.go:4` — *"Money, prices, quantities and L2 depth must NEVER touch float64."* Exact base-10 via `common.v1.Decimal` ↔ `*big.Rat`.

---

## 6. Messaging / IPC

**NATS JetStream** = the live spine (short-retention hot tier). **Kafka** = the durable log of record, **derived from NATS by one archiver** — services never dual-publish (`KANZ_BRAIN.md`, DATA-M1). **gRPC** for venue adapters (`venue.v1.VenueAdapterService`) and the operator (`operator.v1`). **HTTP** at the api-gateway edge.

Bus client: `kanz/pkg/bus/` — `bus.go`, `consumer.go`, `command.go`, `context.go`, `dedup.go`. One `Client` interface over `NATSClient` and `KafkaClient`, so envelope stamping, lineage, dedup and DLQ layer once. Wire framing is a serialized `envelope.v1.EventFrame{envelope=1, payload=2}` sibling layout.

---

## 7. Exchange Integrations

**Two adapters, matching the MVP scope:** `kanz/services/venue-binance/` and `kanz/services/venue-okx/`, each `internal/{binance|okx, config}`.

**Idempotency — the mechanism is centralised, not per-adapter.** A grep for `clOrdId|newClientOrderId|client_order_id` under `kanz/services/venue-*/` returns **zero files**; the mechanism lives in the shared seam: `kanz/internal/execution/{venue.go, closes.go, query.go, grpc_venue.go, fixvenue.go}`. `KANZ_BRAIN.md` states the contract — *"our `order_id` **is** the venue `clOrdId`/`newClientOrderId`, so a duplicate or ambiguous-timeout submit recovers by querying that id rather than double-executing"*, and `fill_id` is deterministic (`{venueSymbol}-{tradeId}`).

**Rate limiting:** 10 files under the two adapters match `rate.?limit`. **Reconnect:** 4 files. **Backoff:** 2 files.

**Transport posture:** the default binary is vendor-free — per `KANZ_BRAIN.md` the websocket links only under `//go:build binance` / `okx` tags, verified via `go list -deps`. I searched `kanz/services/venue-binance/internal/*/*.go` for `go:build` and found no hits at that exact depth; the tags are elsewhere in the tree and I have not pinned the exact files.

---

## 8. Authentication & Authorization

**Two planes, by decision (2026-07-14, `KANZ_BRAIN.md`):**
- **HTTP/API plane** — OIDC/JWT at the api-gateway (Eighred SSO), with a dev HS256 fallback. `kanz/pkg/auth/` holds `oidc.go`, `issuer.go`, `principal.go`, `authz.go`, `audit.go`.
- **Operator plane** — SPIFFE/SVID identity in-cluster; `kanz-halt` runs as a Job because SPIRE issues an SVID to a *pod*, from its ServiceAccount.

**RBAC is a capability model, not role strings.** `authz.Mux.Handle` takes the capability as a **required parameter**, so a route without one does not compile. `Read`/`Trade` are the RO/RW split. An arch test fails the build on any undeclared or under-privileged capital route — `kanz/services/api-gateway/internal/authz/arch_test.go:39-88`.

**Gateway refuses to start** without authentication *and* authorization configured — `kanz/services/api-gateway/internal/config/config.go:149-158`, with the comment *"enforced here the same way — by refusing to run, not by logging a warning that scrolls past."*

**Hierarchy:** the `tenant` claim **is** the hierarchy (binds sub-user → company → `app.tenant_id` → RLS). A `root@company` tier was deliberately **not** built (SOV-01, closed 2026-07-16).

---

## 9. Secret Management

**Design:** memory-only. Vault CSI mounts secrets as files; services read them via a `secret()` helper preferring `<KEY>_FILE` over a plaintext env var (SEC-01d).

**I searched for on-disk secret writes** (`os.WriteFile`/`ioutil.WriteFile` near key/secret identifiers across `kanz/`): the only two hits are in **tests** — `kanz/cmd/kanz-provisioner/join_test.go:68` and `kanz/cmd/universe/model_validation_test.go:275`. **No product-code path writes a secret to disk.** I found no `.env` files in the repository.

**`SetVenueKeys` is write-only by construction** — no value-returning method on the `secrets.Store` interface; `ListVenueKeys` is presence-only.

**A known defect in this layer** (found in this session, fixed for 2 of 17): `secret()` discards the `os.ReadFile` error, so a mounted-but-unreadable Vault CSI secret is indistinguishable from one never configured. Present in all 17 composition roots; fixed in `venue-binance`/`venue-okx`. Tracked as `SEC-CONFIG-001`.

---

## 10. Testing

- **441** `*_test.go` files under `kanz/`.
- **38** architecture guard files in `kanz/test/arch/`.
- Test tiers: `kanz/test/{arch, contract, e2e, load, mtls}`.
- Full suite at time of writing: **175 packages ok, 0 FAIL** (`go test -p 1 ./...`).

**Known coverage hazards, from the board:**
- Postgres-gated tests **skip silently** without `TEST_POSTGRES_URL`, and must run as a **NOSUPERUSER** role or RLS is bypassed and the isolation tests pass falsely.
- `go test ./...` races on a shared `TEST_POSTGRES_URL`; `-p 1` is required.
- `fakeBus` (OMS test double) does **not** validate envelopes, so it accepts what a real broker rejects — this let a Critical tenant-context bug ship past a green suite.

---

## 11. Configuration

Per-service `internal/config/config.go` with a `Load() (Config, error)` constructor — e.g. `kanz/services/oms/internal/config/config.go:154`. Values come from environment variables, with `<KEY>_FILE` taking precedence for secrets.

**There is no environment/profile selector** (no `KANZ_ENV`, no `ENVIRONMENT`). I searched `kanz/services/venue-binance/internal/config/config.go` and `kanz/infra/deploy/venue-binance-deploy.yaml` and found no such marker. Environment separation is expressed by **which values are injected** (manifest + Vault path), not by a mode flag the binary reads.

---

## What this repository can actually do today — 10 honest points

1. **Run the full automated trading loop end-to-end**, cluster-proven: signed TradingView webhook → `StrategySignal` FACT → venue-allocated `SubmitOrder` commands → OMS gate → venue → fills → bitemporal projection. Proven on `kind-kanz-dryrun` with real fills.
2. **Survive a crash mid-order without double-trading.** The OMS reconciles interrupted orders against venue truth, re-drives or quarantines by policy, and the quarantine arm was proven on a cluster with a measured position book (9→14, not 19).
3. **Refuse to trade what it cannot value.** The pre-trade gate rejects unpriced MARKET/STOP orders terminally rather than valuing them at zero — which it used to do, silently bypassing all five compliance rules.
4. **Enforce tenant isolation at the database**, with `FORCE` RLS proven live (an `ENABLE`-only table leaked across tenants; `FORCE` returned 0 rows).
5. **Do exact decimal money arithmetic** with float banned on every capital path, after wrap bugs that admitted a $184bn notional against an $80k book.
6. **Publish a signed, scanned, SBOM-attested 26-image release** — `v0.2.0`, trivy + cosign keyless + SBOM, with a `pin-digests` job that rewrites manifests to immutable digests.
7. **Manage the Kubernetes estate from a TUI** — join a node, cordon, drain (both confirm branches), relabel region, write venue keys — all proven through the TUI against a live two-node k3s cluster.
8. **Halt trading platform-wide** via a broadcast kill signal (`kanz-halt`) that a pod booting tomorrow still learns, with a deny-by-default gate.
9. **Hold ~38 architecture guards** that fail the build on structural regressions — DLQ wiring, tenant scope, image pinning, topic topology, RBAC bounds, SSH-plane containment.
10. **What it cannot do:** it has **never executed a disaster-recovery failover**, has **no running Prometheus** (so every degraded mode is unobservable in production), and **cannot presently say which database cluster the OMS order store is backed up in** — the production DSNs live only in Vault and the repo gives six conflicting answers.

---

## OPEN QUESTIONS (unresolved from the repository alone)

1. **Which database does each service actually use in production?** `ONBOARD-M6` — six conflicting in-repo answers; the truth is a `vault kv get` nobody here can run.
2. **Are `oms`, `tv-sync`, `venue-*`, `regulatory` in any DR cluster?** They hold `*-db` SecretProviderClasses but appear in no cluster in `infra/dr/postgres/` and in no written exclusion.
3. **Where are the venue build tags?** `KANZ_BRAIN.md` says the websocket links only under `//go:build binance|okx`; I did not locate the tagged files at `kanz/services/venue-*/internal/*/*.go`.
4. **Is there a production environment at all?** The only kubectl context found in prior sessions was a kind rig; `kanz-data`, `spire-system` and `vault` namespaces were absent.
