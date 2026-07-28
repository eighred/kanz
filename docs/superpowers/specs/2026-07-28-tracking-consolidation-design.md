# Tracking Consolidation — Design

**Date:** 2026-07-28 · **Status:** approved for planning · **Owner ruling:** GitHub Issues becomes the single board.

## Problem

Five documents each claim to say what to do next, and none defers to the others:
`KANZ_TASKS.md` → TODO, `KANZ_TASKS.md` → PATH TO PARITY, `KANZ_ROADMAP.md`,
`docs/PLAN.md`, `docs/GAP.md`. A sixth, `docs/RELEASE_READINESS.md`, was demoted
to a dated audit on 2026-07-27 but still reads as a status page.

Measured, 2026-07-28:

| Artifact | Size | Assessment |
|---|---|---|
| `KANZ_TASKS.md` | 355 KB / 424 lines | four sections in one file; the `DONE` section alone is 155 KB and was supposed to have been removed on 2026-07-27 |
| `KANZ_BRAIN.md` | 100 KB | architectural memory — the one artifact worth keeping |
| `KANZ_ROADMAP.md` | 14 KB | a third roadmap |
| `DEMO_DEPLOYMENT.md` | 30.7 KB | untouched since 2026-07-26 |
| `docs/RELEASE_READINESS.md` | 14.6 KB | dated audit |
| `docs/{PLAN,GAP,ANALYSIS}.md` | 41.7 KB | **not on `main`** — only on branches behind PRs #52–#54 |
| `docs/superpowers/{plans,specs}` | 35 + 13 files | correct location, keep |
| `kanz/docs/superpowers/` | 6 + 6 files | duplicate tree in the wrong place |
| `.superpowers/sdd/` | 236 files / 5.5 MB | gitignored agent scratch; 171 are per-commit review reports |
| claude-mem DB | 35 MB | automatic; injects 50 observations per session start |
| native memory | 12 files | healthy; unchanged |

The board is not merely large. It is *load-bearing in the wrong direction*: a
355 KB file whose long lines defeat ordinary reading tools is consulted by every
session and by a shell validator, and it disagrees with `docs/PLAN.md` about what
comes next.

## Non-goal

This is tracking hygiene. It ships no product capability and moves nothing toward
Aladdin parity by itself. Its value is that five conflicting roadmaps stop
producing contradictory next-actions.

## Decisions taken

1. **GitHub Issues on `eighred/kanz` is the single board.** Issues are enabled,
   currently **zero** issues exist, and `gh` is authenticated with `repo` scope —
   a clean slate with no migration collision. Issues are not gated by the Actions
   billing halt.
2. **Delete the tracking markdown; keep `KANZ_BRAIN.md`, narrowed.** Git history
   retains everything, so deletion is recoverable.
3. **Clear the agent scratch; leave the claude-mem DB intact.** Deleting the
   `DONE` section makes claude-mem *more* load-bearing, not less — it and git
   history become the only searchable record of the ~96 completed epics. Reduce
   its session injection instead.
4. **Land on a branch behind one PR; close PRs #50–#54.** Branch protection is
   active on this repository (it blocked PR #46 on 2026-07-28), and a ~400 KB
   deletion should be reviewable before it lands.

## Architecture — four layers, no fifth

| Layer | Home | Holds | Written by |
|---|---|---|---|
| What to do | **GitHub Issues** | every open item, with its acceptance command | humans and agents, explicitly |
| Why it is shaped this way | `KANZ_BRAIN.md` | durable architectural decisions and anti-decisions | by hand, rarely |
| What happened | claude-mem + git history | sessions, commits, completed epics | automatically, never by hand |
| Cross-project facts | native memory (`~/.claude/.../memory/`) | preferences, environment setup, working style | by hand, rarely |

No layer duplicates another. `CLAUDE.md` names the layers and stops; it does not
restate their contents, and it does not restate `kanz-forge:engineering-standard`.

### Issue taxonomy

**Milestones** `M0`…`M6`, carried verbatim from `docs/PLAN.md` so the dependency
sequence survives the file's deletion. **No milestone** means unscheduled backlog.

The sequencing constraint that must survive: **M0 first and not partially**, and
**M3 must not precede M0.3** — placing real orders against a store that may not be
backed up is the one ordering error with an unrecoverable failure mode.

**Kind labels** (exactly one per issue):

- `open-work` — nothing built yet.
- `needs-verification` — **code is in; nothing has run it.** This preserves the
  READINESS board's founding premise: track what can be *proven*, not what has
  been written. A document, a roadmap, a comment or a `DONE` line is not evidence.
- `blocked-external` — needs credentials, cluster access, or a human decision
  outside the repository.
- `decision-needed` — a ruling, not a coding task.

**Priority labels:** `P0`, `P1`, `P2`, carried from `docs/GAP.md` §2.

**Area labels:** `ci`, `dr`, `observability`, `security`, `execution`, `tui`,
`infra`, `schemas`, `compliance`.

### Issue body contract

Four fields, not an essay. An issue that cannot state how it will be proven does
not belong on the board.

```
**Evidence:** <file:line, commit sha, or the command whose output is quoted>
**Verified when:** <a runnable command and its expected result>
**Blocked by:** <issue number, credential, or access — or "nothing">
**Source:** <the document this was migrated from, for one release only>
```

## Migration — ~55 issues

| Source | Issues | Note |
|---|---|---|
| `docs/PLAN.md` M0.1–M0.5 | 10 | absorbs `GAP.md` C1–C6 and `ONBOARD-M6`; they are the same work described twice |
| `docs/PLAN.md` M1–M6 | 21 | includes the `KANZ_TASKS.md` OPS-M3/M4a/M4/M5 and OPS-M2f-a…d items, which land in M5 |
| READINESS `IMPLEMENTED BUT UNVERIFIED` rows | **6**, not 19 | grouped by area; 19 separate issues would be noise, and they share one acceptance shape |
| READINESS open / blocked / decision rows | 9 | includes the SSO `portfolios` claim, which is a prerequisite on a person, not on code |
| PATH TO PARITY + SOV backlog | 9 | unscheduled; each sits behind a delivered seam awaiting an external boundary |

The full enumerated list is Appendix A.

### What does not become an issue

`docs/GAP.md` §0 — the five **superseded directions** — moves into
`KANZ_BRAIN.md`'s existing *Anti-decisions* section:

1. Remote node **teardown** from the TUI (operator RBAC forbids node delete by design).
2. Kill-switch RAM/binary **wiping** (a host-vector design that did not survive the 2026-07-16 Kubernetes decision).
3. The `root@company` tenant-admin tier (SOV-01 closed; the `tenant` claim *is* the hierarchy).
4. TradingView **overlay / market-data ingest** (no redistribution API; Reuters/DJ news licensed for TV's own UI only).
5. SSH-fingerprint device registration for the TUI (two-planes decision).

This is the highest-value content in the documents being deleted. It records what
was decided *against*; without it, all five are re-proposed within a month. It
belongs in the architectural-memory layer, not the board — an anti-decision is
not work.

## Deletions

**From `main`:** `KANZ_TASKS.md`, `KANZ_ROADMAP.md`, `DEMO_DEPLOYMENT.md`,
`docs/RELEASE_READINESS.md`, `tools/validate-board.sh` (it validates a file that
will not exist), and the duplicate `kanz/docs/superpowers/` tree — whose 12 files
are moved into `docs/superpowers/` first, preserving their dated filenames.

**Never on `main`, so closed rather than deleted:** `docs/PLAN.md`,
`docs/GAP.md`, `docs/ANALYSIS.md` — mined for issues from the working tree, then
PRs #52, #53, #54 are closed with a comment naming the tracking issue. PRs #50
and #51 edit `docs/RELEASE_READINESS.md` and `KANZ_TASKS.md` and are closed the
same way. **PR #49 is a code fix and is untouched.**

**Untracked:** all 236 files under `.superpowers/sdd/`.

**Rewritten:** `CLAUDE.md`. It currently reproduces the entire `/forge-tasks`
engineering specification, which already exists as `kanz-forge:engineering-standard`.
It becomes a short pointer to the four layers.

## Changes outside the repository

These are part of this work, not a follow-up. Without them every future session
is instructed to consult a board that no longer exists.

1. **`~/.claude/skills/kanz-forge/hooks/session-start.js`** — its emitted routing
   block names "the four legacy boards" and `KANZ_TASKS.md` / `KANZ_BRAIN.md`.
   `eighred-kanz` must be described as Issues-backed.
2. **`~/.claude/skills/kanz-forge/skills/stack-routing/SKILL.md`** — same edit in
   the *Where memory lives* section: `eighred-kanz` leaves the legacy-board list
   and gains a line naming GitHub Issues as its board.
3. **`~/.claude-mem/settings.json`** — `CLAUDE_MEM_CONTEXT_OBSERVATIONS` 50 → 20.
4. **Native memory** — `kanz-main-only-workflow.md` states "commit directly to
   `main`, never open PRs" (2026-07-14). The repository now runs PRs with active
   branch protection. Correct the memory rather than leaving a stale instruction
   that contradicts observable practice.

## Error handling and reversibility

- **Issue creation is the only step that is not a plain `git revert`.** It runs
  before any deletion, and each created issue number is recorded so a failed run
  can be reconciled rather than re-run blindly. Duplicate issues are worse than
  no issues.
- **Deletion happens in a separate commit from issue creation**, so the board can
  be inspected against the documents before anything is removed.
- **Rollback:** revert the PR. Every deleted file returns from git history; the
  issues remain and are simply ahead of the repository.
- **The scratch directory is gitignored and not recoverable.** It contains 171
  per-commit review reports whose findings are already in git history and
  claude-mem. This is accepted, not overlooked.

## Verification

The work is complete when all of the following produce the stated result:

1. `gh issue list --repo eighred/kanz --state open --limit 100 | wc -l` returns
   the created count, and every issue carries exactly one kind label and one
   priority label.
2. Every `P0` item in `docs/GAP.md` §2 (C1–C6) maps to an open issue — checked by
   name, not by count.
3. `gh pr list --repo eighred/kanz --state open` returns **only** #49.
4. `git ls-files | grep -E '(KANZ_TASKS|KANZ_ROADMAP|DEMO_DEPLOYMENT|RELEASE_READINESS|validate-board)'`
   returns nothing.
5. `git ls-files kanz/docs/superpowers` returns nothing, and
   `git ls-files docs/superpowers | wc -l` has grown by 12.
6. `grep -c 'Anti-decisions' KANZ_BRAIN.md` returns 1, and all five §0 items
   appear beneath it.
7. `go build ./... && go test -p 1 ./...` from `kanz/` is green — this change
   touches no Go code, so any failure is pre-existing and must be reported as
   such rather than absorbed.
8. A fresh session's SessionStart block names GitHub Issues and does not name
   `KANZ_TASKS.md`.

---

## Appendix A — the issue inventory

The `#` column is a **sequence number within this appendix**, not a GitHub issue
number — GitHub assigns those at creation, and the repository's PR numbering
already reaches #54. Where this document means a GitHub number it writes `PR #49`
or `issue #<n>` explicitly.

### M0 · Hardening (10)

| # | Title | Kind | Pri | Area |
|---|---|---|---|---|
| 1 | Mirror base images to `ghcr.io/eighred/base/*`; remove the Docker Hub SPOF | open-work | P1 | ci |
| 2 | Scheduled refresh workflow for the base-image mirror | open-work | P2 | ci |
| 3 | Arch guard: every Dockerfile `FROM` resolves inside the mirror | open-work | P1 | ci |
| 4 | `cosign verify` a release digest from a machine that did not build it | needs-verification | P0 | security |
| 5 | Resolve per-service production DSNs (`vault kv get`) — ONBOARD-M6 | blocked-external | P0 | dr |
| 6 | Every `*-db` service in a CNPG cluster or a named exclusion, plus an arch guard | open-work | P0 | dr |
| 7 | Deploy a Prometheus and prove all 26 services plus node-exporter are scraped | open-work | P0 | observability |
| 8 | Delete `data-quality.rules.yaml` and `kanz_data_staleness_lag_seconds` — 8/8 metrics no longer exist | open-work | P0 | observability |
| 9 | Arch guard: every metric named in `alerts/` and `slo/` exists in Go, stripping `_bucket`/`_count`/`_sum` | open-work | P1 | observability |
| 10 | Promote one `secret()` helper, migrate all 17 call sites, guard against local redefinition | open-work | P1 | security |

### M1 · TUI and identity (3)

| # | Title | Kind | Pri | Area |
|---|---|---|---|---|
| 11 | Make `kanz` the single TUI entry point with sub-screens | open-work | P0 | tui |
| 12 | Fold the `universe` operator panes into the `kanz` shell | open-work | P1 | tui |
| 13 | Fold `kanz-monitor` in without breaking the read-only observer guards | open-work | P1 | tui |

### M2 · Baskets (deploy-time) and secrets (2)

| # | Title | Kind | Pri | Area |
|---|---|---|---|---|
| 14 | Document the deploy-time basket contract as the supported path | open-work | P1 | execution |
| 15 | Close the venue-key loop against a real exchange (`OPERATOR_VENUE_PROOF`) | blocked-external | P1 | execution |

### M3 · Execution against a real venue (3)

| # | Title | Kind | Pri | Area |
|---|---|---|---|---|
| 16 | Obtain OKX and Binance **testnet** credentials | blocked-external | P0 | execution |
| 17 | First testnet order through the full chain, landing in `ledger_entries` with a `venue_account_id` | open-work | P0 | execution |
| 18 | Prove idempotency and ambiguous-timeout recovery against a real venue | open-work | P0 | execution |

### M4 · Risk gate (2)

| # | Title | Kind | Pri | Area |
|---|---|---|---|---|
| 19 | Confirm COMP-M2's reference-price source without creating a silent bypass | open-work | P1 | compliance |
| 20 | Auto-unwind decision-path skeleton — decides, does not execute | open-work | P2 | execution |

### M5 · Node management and operability (9)

| # | Title | Kind | Pri | Area |
|---|---|---|---|---|
| 21 | Surface halt and resume in the TUI | open-work | P1 | tui |
| 22 | OPS-M2f-a — labelled provisioning node pool, replacing the control-plane pin | open-work | P1 | infra |
| 23 | OPS-M2f-b — prove a freshly joined node pulls with the `ghcr-pull` credential | needs-verification | P1 | infra |
| 24 | OPS-M2f-c — `test/mtls/up.sh` on Linux with Docker (currently failing) | open-work | P2 | infra |
| 25 | OPS-M2f-d — registry-credential rotation from the TUI | open-work | P2 | tui |
| 26 | OPS-M3 — TUI local-bootstrap; node #1 as an ordinary target | open-work | P1 | tui |
| 27 | OPS-M4a — choose one digest substitution point so signed digests actually deploy | open-work | P0 | infra |
| 28 | OPS-M4 — TUI release lifecycle: version bump and rollback by digest | open-work | P1 | tui |
| 29 | OPS-M5 — remaining operational surfaces: health, logs, restart, per-service DSNs | open-work | P2 | tui |

### M6 · Telemetry (2)

| # | Title | Kind | Pri | Area |
|---|---|---|---|---|
| 30 | Positions, PnL and risk in the TUI from live feeds | open-work | P1 | tui |
| 31 | Grafana or equivalent over the SLO recording rules that already compute | open-work | P2 | observability |

### Verification backlog — grouped, no milestone (6)

Each is `needs-verification`: the code is merged and the test suite is green, but
nothing has executed it against a real dependency.

| # | Title | Pri | Area |
|---|---|---|---|
| 32 | Verify the money-arithmetic fixes: `addDecimal`, `dec.ToProto` capital paths, compliance-gate arithmetic, the `Decimal` domain | P0 | execution |
| 33 | Verify the execution-path fixes: COMP-M2, book-snapshot→MARK confusion, SimVenue MARKET fills, cross-user order ownership | P0 | execution |
| 34 | Verify the `audit` scoping fixes: `?tenant=`, lineage/reports/SOC2 evidence, `verify` authentication | P0 | security |
| 35 | Verify the onboarding and RLS path (ONBOARD-M1…M5, and RLS coverage beyond 5 of ~15 tables) | P1 | security |
| 36 | Verify SPIRE/mTLS and the `infra/` manifests against a real cluster | P1 | infra |
| 37 | Verify the operator control plane and Universe TUI end-to-end — F0+S1 was never cluster-proven | P1 | tui |

### Open work, blocked, and decisions (9)

| # | Title | Kind | Pri | Area |
|---|---|---|---|---|
| 38 | NATS JetStream streams have no durable storage and no re-provisioning path | open-work | P0 | infra |
| 39 | `nats-rebuild` has no tenant dimension — tenant-prefixed topics are outside DR | open-work | P1 | dr |
| 40 | `dec.ToProto` still wraps for ~30 non-capital callers | open-work | P2 | execution |
| 41 | Bus-wide `Decimal` validation — one ingress is bounded, the generic case is not | open-work | P1 | execution |
| 42 | The OMS `market.>` subscription is narrowed but its volume is still unbounded | open-work | P2 | execution |
| 43 | Multi-tenant runtime — provisioning creates no compute | decision-needed | P1 | infra |
| 44 | Does a production environment exist at all? | blocked-external | P0 | infra |
| 45 | Eighred SSO must issue the `portfolios` claim before `96ca4e4` reaches production | blocked-external | P0 | security |
| 46 | Locate the venue build tags — `KANZ_BRAIN.md` claims `//go:build binance\|okx`; they were not found where stated | open-work | P2 | execution |

### Unscheduled backlog — credential- and boundary-gated (9)

| # | Title | Kind | Pri | Area |
|---|---|---|---|---|
| 47 | PARITY-07b — publish target: a tagged `kanz-schemas-go` companion repo and a private GOPROXY | blocked-external | P2 | schemas |
| 48 | PARITY-07c — run the SDK release pipeline to tagged Go/Python/TS artifacts | blocked-external | P2 | schemas |
| 49 | PARITY-07d — pin published versions, delete the `replace` directives, arm the dormant `KANZ_SDK_PUBLISHED` guard. **Blocked on a module-path decision:** the module is `github.com/kanz-eng/kanz` but the repository is `eighred/kanz`; harmless under `replace`, fatal once a version has to resolve | decision-needed | P2 | schemas |
| 50 | Composition-root vendor `Source` bindings: Bloomberg BLPAPI, Refinitiv RTSDK, ICE, anthropic, go-redis | blocked-external | P2 | infra |
| 51 | Licensed datasets: ISDA SIMM, BCBS FRTB, NGFS, the live FX feed, regulator worked examples | blocked-external | P2 | compliance |
| 52 | Real infra and human loops: live Redis, a real DR region, `kanz-books` migrations, the PITR drill, SOC 2 engagement, first-client UAT | blocked-external | P2 | infra |
| 53 | SOV-03 — sovereign telemetry: cross-tenant NAV, growth, fee and slippage for the estate operator | open-work | P2 | execution |
| 54 | SOV-04 — license revocation, built on the `kanz-halt` primitive | open-work | P2 | security |
| 55 | SOV-06 — push telemetry back to TradingView; strategy details must never leave the estate | open-work | P2 | execution |
