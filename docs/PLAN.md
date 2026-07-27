# Execution Plan — Phase 3

**Pinned to:** `origin/main` @ `4ca6a9a`. **Date:** 2026-07-28. **Input:** `docs/GAP.md`.

**How this plan differs from the milestone skeleton in the brief.** The brief proposes M0–M6 as if each were a build. Against this repository four of them are largely *already delivered*, and the honest plan says so and shrinks: **M3 is one config change**, **M4 is one component**, **M6 is an operational step with no code**. Inventing work to fill a milestone shape would fail the only test that matters — *is the repository objectively closer to production than before?*

The real weight is in **M0** (defects that mislead an operator), **M1** (a client nobody can install), and **M2** (the one genuinely missing domain concept, which is blocked on a product decision).

**Sequencing rule.** Every milestone leaves the tree building and green. M0 → M1 → M2 is a hard chain: M2's surface is served by M1's terminal, and M1 must not be built on a tree whose guards mislead. M3/M4/M5 are independent of M1–M2 and may run in parallel with them.

---

## M-1 — Merge triage (must happen first; not a milestone, a gate)

Six unmerged local branches exist and at least one holds a fix this plan would otherwise re-implement (`GAP.md` B3/A1).

| Branch | Holds | Recommendation |
|---|---|---|
| `fix/venue-orderview-durability` | *fail fast on unreadable declared secret mount* — the venue half of **A1** | **Merge before M0-1**, then M0-1 generalises it to the remaining 15 |
| `fix/provisioning-job-ttl` | bounds the provisioning Job's lifetime | Merge — independent |
| `fix/rig-digest-rewrite` | rig digest rewrite | Review, merge |
| `docs/audit-followups` | the 2026-07-27 audit follow-ups incl. the SEC-CONFIG-001 entry | Merge, **then correct its status line** (A1: 0/17 fixed on main, not 2/17) |
| `docs/demote-release-readiness` | demotes `RELEASE_READINESS.md` to a dated audit | Merge — independent |
| `chore/docs-dedupe` | removes the duplicated engineering standard from `CLAUDE.md` (523→36) and DONE history from the board | **Merge.** It is the one change that reduces the two-places-for-one-fact risk the standard forbids. |

**Acceptance:** `git branch --no-merged main` lists only branches with a written reason to stay open.

---

## M0 — Hardening

*No new features. Every item removes something that misleads an operator or a future reviewer.*

### M0-1 · One `secret()` helper that cannot fail open silently — **M · P0**

**Files**
- create `kanz/internal/config/secret.go` — `func Secret(key string) (string, error)`, distinguishing *declared-but-unreadable* (`<KEY>_FILE` set, read fails → error) from *absent* (unset → env fallback).
- modify 17 call sites: `services/{accounting,alternatives,api-gateway,audit,copilot,datamaster,market-data,oms,regulatory,risk-engine,schema-registry,tv-sync,venue-binance,venue-okx,wealth,webhook-ingest}/internal/config/config.go` + `cmd/kanz-migrate/main.go`.
- create `kanz/test/arch/secret_helper_test.go` — **the deliverable**: fails the build on any new package-local `func secret(`.

**Depends on:** M-1 (merge `fix/venue-orderview-durability` first — it already did venue-binance/okx; do not redo it).

**Acceptance**
```
grep -rn "func secret(" kanz/ --include='*.go' | grep -v _test | wc -l      # → 0
go test ./test/arch/ -run TestNoLocalSecretHelper                          # → ok
go build ./... && go test -p 1 ./...                                       # → ok
```
Plus a table test proving each of the three states: unset → env value; set+readable → file value; **set+unreadable → error, not fallback**.

**Rollback:** revert the commit; the helper is additive and call sites are mechanical.
**Risk:** a service that legitimately runs with an unreadable mount will now refuse to start. That is the intent — but stage it: land the helper and guard, deploy, watch, *then* remove the env fallback if a second phase is wanted.

### M0-2 · Retire the venue build-tag fiction — **S · P0**

**Files**
- `.github/workflows/build.yml:52-56` — delete the "SimVenue only / a trading image is `--build-arg GOTAGS=binance,okx`" comment; replace with the Dockerfile's actual contract.
- `kanz/Makefile` — delete `VENUE_TAGS` (`:18-19`), `test-venues` (`:73`), `build-venues` (`:77`); remove `TAGS` from the `release` doc-comment (`:48-57`). Keep `-tags "$(TAGS)"` only if a live tag (`redis`, `anthropic`) needs it.
- `kanz/services/{oms,archiver,lake-sink}/Dockerfile` — drop the vestigial `-tags "${GOTAGS}"` and its `ARG`.
- `KANZ_BRAIN.md:17,44` — replace the per-venue-tag claim with the delivered architecture (out-of-process adapters, `OMS_VENUE_ENDPOINTS`), noting the supersession rather than deleting the history.

**Depends on:** nothing.

**Acceptance**
```
grep -rn "GOTAGS\|VENUE_TAGS\|build-venues\|test-venues" kanz/ .github/ | wc -l   # → 0
go list -deps ./services/venue-binance/cmd/venue-binance | sort > /tmp/a
go list -deps -tags "binance okx" ./services/venue-binance/cmd/venue-binance | sort > /tmp/b
diff /tmp/a /tmp/b                                                # → identical, as it already is
make build && make test
```

**Rollback:** trivial revert. **Risk:** none — proven inert (560 packages either way). This is subtractive.

### M0-3 · The golden route table covers every handler package — **S · P1**

**Files:** `kanz/services/api-gateway/internal/authz/arch_test.go` — register `control.New(nil).Routes(m)` at `:57` and add the 11 `Operate` routes to the `want` map.

**Acceptance**
```
go test ./services/api-gateway/internal/authz/ -run TestTheWholeRouteTableIsDeclared   # → ok, 27 routes
```
Then **prove the guard bites**: add a throwaway route to `control`, confirm the test fails, remove it.

**Rollback:** revert. **Risk:** none.

### M0-4 · Delete alert rules for metrics nothing emits — **S · P2**

**Files:** `kanz/infra/observability/alerts/data-quality.rules.yaml` (8 orphaned metrics), `slo/slo.recording.rules.yaml` (`kanz_data_staleness_lag_seconds`).

**Acceptance:** every metric named in `infra/observability/**` resolves to a `prometheus.*` registration in Go — assert it as an arch guard so it cannot rot again.

### M0-5 · Drop `web-bff` from the image matrix — **S · P2**

Its product surface was cancelled 2026-07-13; every release still signs, scans, SBOMs and digest-pins it. **Keep the source, remove the build.**
**Files:** `.github/workflows/build.yml`, `release.yml` (matrix entries), `kanz/cmd/kanz-pin-digests` expectations if the count is asserted.
**Acceptance:** release matrix goes 26 → 25 images; `go build ./...` still green; no manifest references the dropped image.

---

## M1 — TUI & Identity

*Identity is done. The gap is that nobody can install the client, and that there are three terminals.*

### M1-0 · Decide the terminal topology — **S · P0 · decision, not code**

`kanz`, `universe`, `kanz-monitor` are three surfaces for one operator (`GAP.md` B1). **Recommendation: `kanz` becomes the single user-facing terminal; `universe` stays as the operator-plane terminal; `kanz-monitor` folds into `kanz`.** Rationale: `universe` is on a different identity plane by decision (`KANZ_BRAIN.md:155`) and merging it would be the parallel-system error in reverse. Record in `KANZ_BRAIN.md`. **Everything below assumes this answer; if it changes, M1-2 changes.**

### M1-1 · Ship the client — **M · P0**

Today `make release` builds `./services/...` only; there is no client artifact anywhere.
**Files:** `kanz/Makefile` (a `client` target building `./cmd/kanz` and `./cmd/universe` for linux/darwin/windows × amd64/arm64, `-trimpath -ldflags="-s -w"`); `.github/workflows/release.yml` (attach the six artifacts to the tag, cosign-sign them like the images); `docs/runbooks/install.md`.
**Acceptance**
```
make client && ls bin/kanz_{linux,darwin,windows}_{amd64,arm64}*     # → 6 artifacts
cosign verify-blob --certificate … bin/kanz_linux_amd64              # → verified
# on a clean box, with bin/ on PATH:
kanz            # → launches, prompts device-flow login, no flags
```
**Test strategy:** a CI job that downloads the tagged artifact on ubuntu/macos/windows runners and asserts `kanz --version` exits 0. **This is the acceptance test for requirement #1 — until a runner that never built the repo can run `kanz`, the requirement is not met.**
**Rollback:** artifacts are additive; drop the release step.
**Risk:** Windows signing/notarisation is out of scope here and will surface as a SmartScreen prompt — call it out, do not silently ship.

### M1-2 · `kanz` becomes a full-screen TUI — **L · P1**

**Reuse, do not invent:** `cmd/universe/{model,view,form,poller}.go` is the delivered bubbletea pattern.
**Files:** `cmd/kanz/internal/repl/` → a `tui/` package (`model.go`, `view.go`, `poller.go`); keep `gateway/client.go` and `tokenstore` unchanged; keep the 8 slash commands as the command palette so nothing regresses.
**Depends on:** M1-0, M1-1.
**Acceptance:** each existing slash command reachable by keyboard navigation; `go test ./cmd/kanz/...` green; a `teatest`-style golden-frame test per screen.
**Rollback:** keep the REPL entrypoint behind `kanz --repl` for one release.
**Risk/unknown:** the brief says "feel like Claude Code". That is a design target, not an acceptance criterion — **it needs a mockup before this starts**, or it will be rebuilt twice.

---

## M2 — Baskets & Secret Vault

*The secret vault half is delivered (`GAP.md` #6). Baskets are the single largest gap in the system.*

### M2-0 · BLOCKING DECISION — is basket allocation runtime or deploy-time? — **P0**

`KANZ_BRAIN.md:152` records that fund-basket allocation is **deploy-time**, on SOV-02's reasoning: *"an account bound to two portfolios must refuse to START, which an API that mutates the binding at runtime re-opens."* Requirement #5 asks for TUI-driven basket CRUD, which is runtime by definition.

**These cannot both be true.** Options:
- **(a) Keep deploy-time.** Baskets are GitOps-committed; the TUI *reads* them and proposes changes as commits. Preserves the refuse-to-start invariant. Cheaper, and consistent with "config changes by attributable commit, not by HTTP".
- **(b) Move to runtime.** Requires re-opening SOV-02 with a new invariant that survives mutation (e.g. binding changes are versioned and a running account rejects a mid-flight rebind). This also unblocks requirement #17 (tenant admin), which the same entry gates on the same decision.

**Recommendation: (a) for MVP, (b) only when a tenant needs self-service** — which is precisely the re-open trigger the decision names. **Nothing below M2-0 should start before this is answered.**

### M2-1 · The basket entity — **L · P0 · blocked on M2-0**

**Files (shape assumes (a)):** `kanz-schemas/proto/portfolio/v1/basket.proto`; a `basket` migration under the owning service (**not** a new service — reuse `risk-engine`'s portfolio ownership or `datamaster`'s reference-data ownership; decide by which one already owns portfolio identity); `/v1/portfolios` read routes + the golden table entry; TUI screens in `cmd/kanz`.
**Acceptance:** a basket with name, allocated capital (`common.v1.Money`, never float) and target weights is visible in `kanz`, drives an order through the existing signal→gate→OMS chain, and its isolation invariant (`internal/execution/router.go:63`) is asserted by a test that fails if Basket A's order can reach Basket B's credential.
**Risk:** this is the item most likely to grow a parallel system. It must extend the existing portfolio identity, not introduce a second one.

---

## M3 — Execution

**Adapters, order lifecycle, idempotency and the bitemporal ledger are all delivered** (`GAP.md` #10, #12, #13; §5). The only gap is one asset.

### M3-1 · Add OKB — **S · P1**

**Files:** `infra/deploy/market-ingest-deploy.yaml:106,113` (`MARKET_INGEST_INSTRUMENTS`, `MARKET_INGEST_OKX_SYMBOLS`), `venue-okx-deploy.yaml:155` (`OKX_SYMBOLS`). **Not** `venue-binance-deploy.yaml` — OKB is OKX-listed.
**Acceptance:** OKB quotes appear on the market-data topic; an OKB order routes to the OKX adapter; **a Binance-routed OKB order is a hard router error, not a silent fallback** (`oms/Dockerfile:22` — *"a MIC with no adapter is a hard error at the router rather than a quiet simulation"*).
**Test strategy:** OKX sandbox. **Unknown to resolve first:** does the reference-data/pricing path (`datamaster`) have an OKB entry? If not, the pre-trade gate will terminally refuse the order — correctly — and this becomes M rather than S.

---

## M4 — Risk Gate

**The pre-trade gate exists and is wired** (`oms/cmd/oms/main.go:208,230`). Two additions.

### M4-1 · Position limits as a pre-trade rule — **M · P1**
Extend `internal/compliance/rules.go`; do **not** add a second gate. **Acceptance:** an order breaching a configured limit is refused at `PhasePreTrade` with a typed reason, and the refusal reaches the client through the existing refusal contract (`docs/superpowers/specs/2026-07-19-refusal-contract-design.md`).

### M4-2 · Auto-unwind skeleton — **M · P2**
An actor that *proposes* an unwind order through the same chain — never a side path to a venue. **Acceptance:** breaching a limit emits an unwind proposal FACT; execution stays behind an explicit affirmative flag, per the never-fabricate rule (`KANZ_BRAIN.md:183`). **Do not let this become an automated trader in MVP.**

---

## M5 — Node management & kill-switch

**Provisioning, lifecycle and the halt broadcast are delivered** (`GAP.md` #7, #9). One RPC is missing; one requirement is cancelled.

### M5-1 · `RemoveNode` — **M · P1**
**Files:** `kanz-schemas/proto/operator/v1/operator.proto` (12th RPC); `services/operator/`; `services/api-gateway/internal/control/control.go` (12th `Operate` route) **+ the golden table** (M0-3 must land first); `cmd/universe`.
**Semantics are already decided** — `KANZ_BRAIN.md:154`: revocation is *stop the workloads, purge the Vault-CSI secrets, scale the tenant to zero*. Implement that; **do not implement host wipe**.
**Acceptance:** on a kind rig, `RemoveNode` drains, deletes the node object, and leaves no Vault-CSI secret bound to it; a second call is idempotent.
**Rollback:** the RPC is additive; unregister the route.

### M5-2 · Binary/RAM/DB wipe — **NOT PLANNED**
Cancelled with the SSH deployment model (`KANZ_BRAIN.md:154`). Re-opening it means re-opening that decision. Listed so it is visibly *decided*, not forgotten.

---

## M6 — Telemetry (TradingView)

**Nothing to build.** `services/tv-sync/internal/brokerapi/` implements the TradingView **Broker API** and 5 `Read` routes serve it (`GAP.md` #14). The brief's feasibility investigation (Pine Script? webhook direction?) is moot — a stronger third option was chosen and shipped.

**M6-1 · Operational only — S · P2:** apply to TradingView for broker-integration whitelisting; document the endpoint contract in a runbook; confirm no strategy-bearing field is exposed (a test asserting the Broker API response shape carries positions/PnL and nothing from the signal or rule engine).

---

## Risks and unknowns, consolidated

| # | Unknown | Blocks | Resolve by |
|---|---|---|---|
| 1 | Runtime vs deploy-time basket allocation | **M2 entirely**, and requirement #17 | Product decision (M2-0) |
| 2 | "Feels like Claude Code" has no acceptance criterion | M1-2 | A mockup before implementation |
| 3 | Does `datamaster` carry OKB reference data? | M3-1 sizing | One query against the datamaster store |
| 4 | Production DB topology / DR membership / does a production environment exist? | Any go-live claim | `vault kv get`; cluster inspection — **not answerable from the repository** |
| 5 | Windows code-signing for the client | M1-1 completeness | Out of scope; must be named, not silently shipped |

## What this plan deliberately does NOT do

- Does not build a `root@company` tier (#17) — decided against, and gated on M2-0.
- Does not build host wipe (#9) — cancelled architecture.
- Does not build SSH-fingerprint identity (#2) — rejected in favour of OIDC.
- Does not delete the frozen analytics (~3,250 LOC) — freezing is cheap and correct; deletion is a large irreversible change with no MVP payoff.
- Does not add a task for formatting, renaming or test-only work.

---

**Phase 3 ends here.** The board (`KANZ_TASKS.md`) and roadmap (`KANZ_ROADMAP.md`) are updated from this plan, not from the brief.
