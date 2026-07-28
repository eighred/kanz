# Tracking Consolidation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Collapse five competing tracking documents into GitHub Issues as the single board, keeping `KANZ_BRAIN.md` as the only architectural-memory file.

**Architecture:** Four layers, no fifth — GitHub Issues (what to do), `KANZ_BRAIN.md` (why it is shaped this way), claude-mem plus git history (what happened), native memory (cross-project facts). Issues are created first from a one-shot migration script so the board can be inspected against the documents *before* anything is deleted; deletion is a separate commit and a plain `git revert` away.

**Tech Stack:** `gh` CLI (authenticated as `akifgrape`, `repo` scope), Git Bash, POSIX shell. No application code changes.

## Global Constraints

- **Repository:** `eighred/kanz` (private). Branch: `chore/tracking-consolidation`, already created off `main`, already carrying the spec at `f0d8652`.
- **Spec:** `docs/superpowers/specs/2026-07-28-tracking-consolidation-design.md`. Appendix A is the authoritative issue inventory.
- **Scratchpad:** `C:\Users\root\AppData\Local\Temp\claude\C--Users-root-Desktop-eighred-kanz\5fc69878-ece8-4fe2-ac5d-01f328c432d5\scratchpad` — referred to below as `$SCRATCH`. The migration script lives here and is **never committed**; it is one-shot, not repo tooling.
- **`gh` lacks `project` scope.** Do not attempt `gh project` commands. Milestones and labels only. If a Projects board is wanted later the user runs `gh auth refresh -s project` themselves.
- **Issue creation is the only non-revertible step.** It runs before any deletion. Every created issue number is appended to `$SCRATCH/created.log` so a partial failure is reconciled, never blind-re-run. **Duplicate issues are worse than no issues.**
- **This change touches no Go code.** If `go build` or `go test` fails at any point, it is pre-existing — report it as such, do not absorb it and do not fix it here.
- **Commit messages** end with `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- **Do not merge the PR.** Task 12 opens it and stops.

## File Structure

| Path | Change | Responsibility after |
|---|---|---|
| `$SCRATCH/create-issues.sh` | create (uncommitted) | one-shot migration of 55 issues; idempotent by title |
| `$SCRATCH/created.log` | create (uncommitted) | reconciliation record: `<number>\t<title>` |
| `KANZ_BRAIN.md` | modify | the only architectural-memory file; gains 3 anti-decisions |
| `CLAUDE.md` | rewrite | short pointer to the four layers; no restated engineering standard |
| `KANZ_TASKS.md` | delete | — |
| `KANZ_ROADMAP.md` | delete | — |
| `DEMO_DEPLOYMENT.md` | delete | — |
| `docs/RELEASE_READINESS.md` | delete | — |
| `tools/validate-board.sh` | delete | validates a file that will not exist |
| `kanz/docs/superpowers/**` (12 files) | move → `docs/superpowers/` | single plans/specs location |
| `~/.claude/skills/kanz-forge/hooks/session-start.js` | modify | routing block stops naming `KANZ_TASKS.md` for this repo |
| `~/.claude/skills/kanz-forge/skills/stack-routing/SKILL.md` | modify | same edit, kept consistent with the hook |
| `~/.claude-mem/settings.json` | modify | `CLAUDE_MEM_CONTEXT_OBSERVATIONS` 50 → 20 |
| `~/.claude/projects/C--Users-root-Desktop-eighred-kanz/memory/kanz-main-only-workflow.md` | rewrite | corrects a stale instruction contradicted by branch protection |
| `.superpowers/sdd/**` (236 files) | delete | gitignored scratch; not recoverable, accepted |

---

### Task 1: Label and milestone scaffolding

Creates the vocabulary every issue depends on. Nothing else can run until this is green.

**Files:**
- No repository files. GitHub metadata only.

**Interfaces:**
- Consumes: nothing.
- Produces: labels `open-work`, `needs-verification`, `blocked-external`, `decision-needed`, `P0`, `P1`, `P2`, `ci`, `dr`, `observability`, `security`, `execution`, `tui`, `infra`, `schemas`, `compliance`; milestones `M0`…`M6`. Task 2's script references these exact strings.

- [ ] **Step 1: Confirm the clean slate**

The migration assumes zero pre-existing issues. If this is not 0, STOP and report — the idempotency assumption in Task 2 no longer holds.

```bash
gh issue list --repo eighred/kanz --state all --limit 200 --json number | tr -d '[]' | wc -c
```

Expected: `1` (an empty JSON array, i.e. no issues).

- [ ] **Step 2: Create the kind labels**

```bash
gh label create open-work         --repo eighred/kanz --color 1d76db --description "Nothing built yet" --force
gh label create needs-verification --repo eighred/kanz --color fbca04 --description "Code is in; nothing has run it" --force
gh label create blocked-external  --repo eighred/kanz --color b60205 --description "Needs credentials, access, or a human outside the repo" --force
gh label create decision-needed   --repo eighred/kanz --color 5319e7 --description "A ruling, not a coding task" --force
```

- [ ] **Step 3: Create the priority and area labels**

```bash
gh label create P0 --repo eighred/kanz --color b60205 --description "MVP blocker" --force
gh label create P1 --repo eighred/kanz --color d93f0b --description "Important, not blocking" --force
gh label create P2 --repo eighred/kanz --color fef2c0 --description "Backlog" --force

for a in ci dr observability security execution tui infra schemas compliance; do
  gh label create "$a" --repo eighred/kanz --color c5def5 --description "Area: $a" --force
done
```

- [ ] **Step 4: Create the milestones**

`gh` has no `milestone` subcommand; this goes through the REST API. `|| true` makes a re-run safe — GitHub returns 422 on a duplicate title.

```bash
gh api repos/eighred/kanz/milestones -f title=M0 -f description="Hardening — no new features. Everything after this is verified by a pipeline that cannot currently complete a merge unaided." || true
gh api repos/eighred/kanz/milestones -f title=M1 -f description="TUI and identity — composition, not new capability." || true
gh api repos/eighred/kanz/milestones -f title=M2 -f description="Baskets (deploy-time) and secrets." || true
gh api repos/eighred/kanz/milestones -f title=M3 -f description="Execution against a real venue. MUST NOT precede M0.3 (issue: DR coverage)." || true
gh api repos/eighred/kanz/milestones -f title=M4 -f description="Risk gate hardening once real orders flow." || true
gh api repos/eighred/kanz/milestones -f title=M5 -f description="Node management, operability, and the kill-switch surface." || true
gh api repos/eighred/kanz/milestones -f title=M6 -f description="Telemetry. Depends on M0 observability and the M1 TUI shell." || true
```

- [ ] **Step 5: Verify**

```bash
gh label list --repo eighred/kanz --limit 100 | grep -cE '^(open-work|needs-verification|blocked-external|decision-needed|P0|P1|P2|ci|dr|observability|security|execution|tui|infra|schemas|compliance)\b'
gh api repos/eighred/kanz/milestones --jq '.[].title' | sort | tr '\n' ' '
```

Expected: `16`, then `M0 M1 M2 M3 M4 M5 M6 `.

No commit — this task changes no files.

---

### Task 2: Write the migration script

Builds the whole board as data, then proves it renders 55 issues **without creating any**. This is the reviewable artifact: a mistake caught here costs nothing, a mistake caught after creation costs a manual cleanup of 55 issues.

**Files:**
- Create: `$SCRATCH/create-issues.sh`

**Interfaces:**
- Consumes: the labels and milestones from Task 1, by exact string.
- Produces: `create-issues.sh` accepting `--dry-run` and an optional milestone filter (`M0`, `M1`…`M6`, or `none` for unmilestoned). Appends `<number>\t<title>` to `$SCRATCH/created.log` on each real creation. Tasks 3 and 4 invoke it.

- [ ] **Step 1: Write the script**

First export the scratchpad path in the shell you will use for Tasks 2–4:

```bash
export SCRATCH="/c/Users/root/AppData/Local/Temp/claude/C--Users-root-Desktop-eighred-kanz/5fc69878-ece8-4fe2-ac5d-01f328c432d5/scratchpad"
mkdir -p "$SCRATCH"
```

Create `$SCRATCH/create-issues.sh` with exactly this content:

```bash
#!/usr/bin/env bash
# One-shot migration of the kanz tracking documents into GitHub Issues.
# NOT repo tooling. Run from anywhere. Idempotent by issue title.
#
#   ./create-issues.sh --dry-run          # render every issue, create nothing
#   ./create-issues.sh --dry-run M0       # render only the M0 issues
#   ./create-issues.sh M0                 # create the M0 issues
#   ./create-issues.sh                    # create every issue not already in created.log
#
# Titles are the idempotency key. A title already present in created.log is
# skipped, so a partial run is resumed rather than duplicated.
set -euo pipefail

REPO=eighred/kanz
LOG="$(dirname "$0")/created.log"
touch "$LOG"

DRY=0
FILTER=""
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY=1 ;;
    *) FILTER="$arg" ;;
  esac
done

COUNT=0

# mkissue <milestone|none> <labels-csv> <title> <evidence> <verified-when> <blocked-by> <source>
mkissue() {
  local ms="$1" labels="$2" title="$3" evidence="$4" verified="$5" blocked="$6" source="$7"

  if [ -n "$FILTER" ] && [ "$ms" != "$FILTER" ]; then return 0; fi

  if cut -f2- "$LOG" | grep -Fxq "$title"; then
    echo "SKIP (already created): $title"
    return 0
  fi

  local body
  body=$(printf '**Evidence:** %s\n\n**Verified when:** %s\n\n**Blocked by:** %s\n\n**Source:** %s\n' \
    "$evidence" "$verified" "$blocked" "$source")

  COUNT=$((COUNT + 1))

  if [ "$DRY" = "1" ]; then
    echo "--- [$COUNT] milestone=$ms labels=$labels"
    echo "TITLE: $title"
    echo "$body"
    return 0
  fi

  local args=(--repo "$REPO" --title "$title" --body "$body" --label "$labels")
  if [ "$ms" != "none" ]; then args+=(--milestone "$ms"); fi

  local url num
  url=$(gh issue create "${args[@]}")
  num="${url##*/}"
  printf '%s\t%s\n' "$num" "$title" >>"$LOG"
  echo "created #$num  $title"
}

# ---------------------------------------------------------------- M0 (10)

mkissue M0 "open-work,P1,ci" \
  "Mirror base images to ghcr.io/eighred/base/* and remove the Docker Hub SPOF" \
  "Seven registry-1.docker.io timeouts on 2026-07-27, three of them consecutive PRs each needing a manual re-run (PRs #46, #48, #49)." \
  "\`grep -rE '^FROM ' kanz --include=Dockerfile* | grep -vc 'ghcr.io'\` returns 0, and ten consecutive kanz-build runs on main show zero registry-timeout failures." \
  "nothing" \
  "docs/PLAN.md M0.1 / docs/GAP.md C5"

mkissue M0 "open-work,P2,ci" \
  "Scheduled refresh workflow for the base-image mirror" \
  "A mirror without a refresh path silently ages; base images stop receiving security updates while every Dockerfile still points at them." \
  "A scheduled workflow re-syncs the mirror and its most recent run is green. Ship it with the mirror, not after." \
  "the base-image mirror issue" \
  "docs/PLAN.md M0.1 risk"

mkissue M0 "open-work,P1,ci" \
  "Arch guard: every Dockerfile FROM resolves inside the mirror" \
  "Nothing prevents a new Dockerfile reintroducing a Docker Hub FROM after the mirror lands." \
  "A test in kanz/test/arch fails the build on a FROM outside the mirror, carries a named-exemption map, and is mutation-proven by flipping one FROM and observing the failure." \
  "the base-image mirror issue" \
  "docs/PLAN.md M0.1 guard"

mkissue M0 "needs-verification,P0,security" \
  "cosign verify a release digest from a machine that did not build it" \
  "release.yml's verify step uses \${{ github.repository }} for --certificate-identity-regexp, so it cannot disagree with what just signed. Signatures are produced and never independently verified." \
  "On a machine that did not build the image: \`cosign verify ghcr.io/eighred/oms@sha256:<digest> --certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-identity-regexp '^https://github.com/eighred/kanz/.github/workflows/release.yml@refs/tags/v.*\$'\` exits 0, and \`cosign download sbom\` returns a non-empty SBOM. Paste the transcript." \
  "nothing — this is executable today and cannot be closed from inside CI" \
  "docs/PLAN.md M0.2 / docs/GAP.md C6 — one of the two standing P0 release criteria"

mkissue M0 "blocked-external,P0,dr" \
  "Resolve the per-service production DSNs (ONBOARD-M6)" \
  "The repository gives six conflicting answers for its own database names: infra/dr/postgres/README.md says one-per-service, cluster.yaml's spec implies CNPG's default \`app\`, infra/security/secrets/README.md:64 says \`risk\`, dev/docker-compose.yml says \`kanz\`, CI TEST_POSTGRES_URL says \`kanzapp\`. The truth is in Vault." \
  "\`vault kv get -field=dsn kv/kanz/oms\` (and per service) returns the deployed DSN, and infra/dr/postgres/README.md plus cluster.yaml's comment are corrected to agree with it." \
  "Vault access, which does not exist on the current box — the only kubectl context is a kind rig with no vault namespace" \
  "KANZ_TASKS.md ONBOARD-M6 / docs/PLAN.md M0.3"

mkissue M0 "open-work,P0,dr" \
  "Every *-db service is in a CNPG cluster or a named exclusion, with an arch guard" \
  "infra/dr/postgres/cluster.yaml defines 3 CNPG clusters; the README maps 6 services and excludes 2 with a written reason. Five services holding *-db SecretProviderClasses are named nowhere: oms, tv-sync, venue-binance, venue-okx, regulatory. After a failover the OMS would start against an empty order store and SweepInterrupted would log count=0 — the same line a healthy clean start produces." \
  "An arch guard passes with every one of the 13 migration-owning services either in a cluster carrying the WAL+PITR contract or in a named exclusion with a written reason; infra/dr/postgres/README.md maps all 13." \
  "the ONBOARD-M6 DSN issue" \
  "docs/GAP.md C1 / docs/PLAN.md M0.3"

mkissue M0 "open-work,P0,observability" \
  "Deploy a Prometheus and prove all 26 services plus node-exporter are scraped" \
  "infra/observability/ ships alerts/, dashboards/, slo/ and a node-exporter.yaml declaring only a Namespace and DaemonSet. No kind: Prometheus, no prometheus.io/scrape, no prom/prometheus image anywhere in infra/. The only rule_files: is slo/slo_test.yaml, a promtool test harness. Every degraded mode is unobservable." \
  "A Prometheus target list showing all 26 services plus node-exporter, and kanz_venue_orderview_durable visible in a query." \
  "nothing" \
  "docs/GAP.md C2 / docs/PLAN.md M0.4"

mkissue M0 "open-work,P0,observability" \
  "Delete data-quality.rules.yaml and the staleness SLO input — the metrics no longer exist" \
  "All 8 metrics in alerts/data-quality.rules.yaml are absent from the Go code; they were emitted by internal/integrity, deleted in DATA-M4. DataQualityMetricsMissing is absent(kanz_data_quality_events_total), so the one rule that can fire would fire forever. slo/slo.recording.rules.yaml consumes kanz_data_staleness_lag_seconds, the same orphan." \
  "\`promtool check rules\` is clean over infra/observability/{alerts,slo} and no rule references a metric absent from the Go source." \
  "nothing" \
  "docs/GAP.md C3 / docs/PLAN.md M0.4"

mkissue M0 "open-work,P1,observability" \
  "Arch guard: every metric named in alerts/ and slo/ exists in Go" \
  "Nothing prevents a second generation of rules outliving the code that emits them, which is exactly how C3 happened." \
  "The guard passes on correct configuration and fails when a metric name is removed from Go. It MUST strip _bucket/_count/_sum before matching — those are Prometheus-generated, never declared in Go, and a guard that is red on correct configuration gets deleted. Doing this by hand produced four false positives." \
  "the dead-rules deletion issue" \
  "docs/PLAN.md M0.4 guard"

mkissue M0 "open-work,P1,security" \
  "Promote one secret() helper and migrate all 17 composition roots" \
  "Every service plus cmd/kanz-migrate has its own \`func secret(k string)\` and all 17 discard os.ReadFile's error, so a mounted-but-unreadable Vault CSI secret is indistinguishable from one never configured. Fixed in venue-binance and venue-okx only — they are the reference. Calibration: the two highest-risk fail-open paths are safe; api-gateway/internal/config/config.go:149-158 refuses to start without auth, and OIDCIssuer is read via plain os.Getenv at :113, not secret()." \
  "\`grep -rc 'func secret(' kanz --include=*.go\` returns 1, and an arch guard forbidding a local secret() is mutation-proven." \
  "nothing" \
  "docs/GAP.md C4 / docs/PLAN.md M0.5 — highest blast radius in M0; land it alone, not alongside behaviour changes"

# ---------------------------------------------------------------- M1 (3)

mkissue M1 "open-work,P0,tui" \
  "Make kanz the single TUI entry point with sub-screens" \
  "Four terminal binaries exist where the target wants one: kanz (REPL, cmd/kanz/main.go:1-6), universe (operator panes, cmd/universe/main.go:1-8), kanz-monitor (live feeds), kanz-halt (brake). All four already speak to the same gateway edge, so this is composition, not new capability." \
  "\`kanz\` launched from bash, zsh, PowerShell and CMD opens the TUI with no flags, and every pane is reachable by keyboard." \
  "nothing" \
  "docs/PLAN.md M1 / docs/GAP.md §1 rows 1 and 4"

mkissue M1 "open-work,P1,tui" \
  "Fold the universe operator panes into the kanz shell" \
  "cmd/universe holds the node/cluster estate, provisioning, node lifecycle and venue-key panes that §1.2 expects to reach from kanz." \
  "Every universe pane is reachable from the kanz shell and \`go build ./cmd/universe\` still succeeds — the standalone binary stays until the shell is proven, so each step is revertable." \
  "the single-entry-point issue" \
  "docs/PLAN.md M1"

mkissue M1 "open-work,P1,tui" \
  "Fold kanz-monitor in without breaking the read-only observer guards" \
  "kanz-monitor is arch-guarded as a read-only observer by TestReadOnlyObserversNeverJoinAQueueGroup and TestReadOnlyObserversCannotPublish. Folding it into a binary that also writes must not let it join a durable consumer group or gain publish rights." \
  "Both guards still pass with the monitor panes inside the kanz shell. Those guards are the acceptance criteria, not a formality." \
  "the single-entry-point issue" \
  "docs/PLAN.md M1 risk"

# ---------------------------------------------------------------- M2 (2)

mkissue M2 "open-work,P1,execution" \
  "Document the deploy-time basket contract as the supported path" \
  "Owner ruling 2026-07-27: baskets stay a deploy-time concern, no runtime CRUD. Portfolio-to-exchange-account binding is SOV-02's deploy-time contract, and the OMS refuses to start when two portfolios share an account. A runtime CRUD API would re-open that refusal." \
  "A documented deploy-time basket change reaches a running OMS, and the OMS refuses to start when two portfolios share an exchange account. That refusal is the feature, not a bug." \
  "nothing" \
  "docs/PLAN.md M2 — supersedes docs/GAP.md §1 row 5, which predates the ruling"

mkissue M2 "blocked-external,P1,execution" \
  "Close the venue-key loop against a real exchange (OPERATOR_VENUE_PROOF)" \
  "S4b built pre-write account proof against the exchange, but OPERATOR_VENUE_PROOF is off — it has never run against a live venue." \
  "A venue key entered in the TUI is proved against the exchange and the adapter logs its resolved account id." \
  "Binance testnet credentials" \
  "docs/PLAN.md M2"

# ---------------------------------------------------------------- M3 (3)

mkissue M3 "blocked-external,P0,execution" \
  "Obtain OKX and Binance testnet credentials" \
  "Every cluster proof to date used SimVenue. Nothing has ever executed against OKX or Binance. This blocks all of M3 and the M2 venue-key proof." \
  "Credentials exist and are reachable by the venue adapters in a test environment." \
  "an account decision outside the repository" \
  "docs/PLAN.md open question 4"

mkissue M3 "open-work,P0,execution" \
  "First testnet order through the full chain, landing in ledger_entries with a venue_account_id" \
  "The trading loop is cluster-proven end-to-end, but only against SimVenue, which fills at limit and has never been wrong in a way that mattered — yet." \
  "A BTC, ETH or OKB order placed on testnet through webhook-ingest → signal → OMS gate → venue → fills → projection, appearing in ledger_entries with a venue_account_id." \
  "the testnet credentials issue, and the DR coverage issue — M3 must not precede M0.3, because placing real orders against a store that may not be backed up is the one ordering error with an unrecoverable failure mode" \
  "docs/PLAN.md M3"

mkissue M3 "open-work,P0,execution" \
  "Prove idempotency and ambiguous-timeout recovery against a real venue" \
  "internal/execution makes our order_id the venue clOrdId/newClientOrderId and fill_id deterministic as {venueSymbol}-{tradeId}, with a 1500 ms in-flight timeout on the healing seam. The mechanism is centralised, not per-adapter — a grep under services/venue-*/ returns zero files." \
  "A deliberate duplicate submit produces exactly ONE exchange order, and an ambiguous timeout is recovered by a clOrdId query rather than a re-submission." \
  "the first testnet order issue" \
  "docs/PLAN.md M3"

# ---------------------------------------------------------------- M4 (2)

mkissue M4 "open-work,P1,compliance" \
  "Confirm COMP-M2's reference-price source without creating a silent bypass" \
  "COMP-M1 deliberately made unpriced MARKET orders reject terminally with PRICE_UNAVAILABLE rather than valuing them at zero, which had been silently bypassing all five compliance rules. Wiring a price source must not undo that." \
  "A MARKET order against a governed portfolio is admitted once a reference price exists, and is STILL refused without one. The continued refusal is the guard." \
  "nothing" \
  "docs/PLAN.md M4"

mkissue M4 "open-work,P2,execution" \
  "Auto-unwind decision-path skeleton — decides, does not execute" \
  "Phase 1.6 lists auto-deleveraging as long-term; only the skeleton is in MVP scope." \
  "A breach triggers the unwind skeleton's decision path without executing any order." \
  "nothing" \
  "docs/PLAN.md M4"

# ---------------------------------------------------------------- M5 (9)

mkissue M5 "open-work,P1,tui" \
  "Surface halt and resume in the TUI" \
  "cmd/kanz-halt broadcasts a kill signal on a compacted stream that a pod booting tomorrow still learns, with a deny-by-default gate. It works, but it is a separate binary." \
  "Halt and resume are driven from the TUI and the platform is observed stopping and restarting." \
  "nothing" \
  "docs/PLAN.md M5 / docs/GAP.md §1 row 10"

mkissue M5 "open-work,P1,infra" \
  "OPS-M2f-a — labelled provisioning node pool, replacing the control-plane pin" \
  "OPS-M2e pinned the operator Deployment and both Jobs to the control plane. That MOVED the eviction deadlock rather than removing it: draining the k3s server makes Add Node and Test Connection unschedulable." \
  "The operator and both Jobs schedule on the labelled pool and NOT on ordinary workers, asserted by the existing TestProvisioningJobsArePinnedTo* guard; and the k3s server can be drained while Add Node still succeeds." \
  "nothing" \
  "KANZ_TASKS.md OPS-M2f-a / docs/PLAN.md M5. Risk: the provisioning Job mounts cluster-admission credentials — any placement change must preserve credential confinement, which is why it is pinned today."

mkissue M5 "needs-verification,P1,infra" \
  "OPS-M2f-b — prove a freshly joined node pulls with the ghcr-pull credential" \
  "imagePullSecrets are attached at 26 ServiceAccounts and guarded, but the runtime half has never been observed. This is the one thing OPS-M2e explicitly did not prove." \
  "A node provisioned through the TUI runs a platform workload with NO manual image step, proven by scheduling one onto it immediately after join." \
  "nothing" \
  "KANZ_TASKS.md OPS-M2f-b / docs/PLAN.md M5"

mkissue M5 "open-work,P2,infra" \
  "OPS-M2f-c — test/mtls/up.sh on Linux with Docker, currently failing" \
  "Resolved as FAILING on 2026-07-27; it is no longer an unverified check but a known-broken one." \
  "\`sh test/mtls/up.sh\` reaches its \"Server is ready\" check on a Linux host with Docker and the tenancy assertions pass." \
  "a Linux host with Docker" \
  "KANZ_TASKS.md OPS-M2f-c"

mkissue M5 "open-work,P2,tui" \
  "OPS-M2f-d — registry-credential rotation from the TUI" \
  "Deferred deliberately out of OPS-M2f-b on 2026-07-26: that item's verified-when never asked for rotation." \
  "A token entered in the TUI replaces ghcr-pull in all three namespaces, and a pod that was failing ErrImagePull on the old credential reaches Running." \
  "the OPS-M2f-b image-pull issue" \
  "KANZ_TASKS.md OPS-M2f-d"

mkissue M5 "open-work,P1,tui" \
  "OPS-M3 — TUI local-bootstrap: node #1 as an ordinary target" \
  "Direct SSH from the TUI: install k3s server, install SPIRE and the operator, so the estate can be created rather than only extended." \
  "From a bare host and an empty cluster list, the TUI alone reaches a Ready node running the operator, and a SECOND node then joins it through the existing Add Node path." \
  "nothing" \
  "KANZ_TASKS.md OPS-M3"

mkissue M5 "open-work,P0,infra" \
  "OPS-M4a — choose one digest substitution point so signed digests actually deploy" \
  "The pipeline signs a digest and nothing deploys it. A second independent gate found 2026-07-26: release.yml's matrix is [risk-engine, schema-registry] — two services — while build.yml's is 26. TestProductionManifestsPinImagesByDigest shipped 2026-07-27 with seven mutation-proven arms and found a live regression on its first run." \
  "\`kubectl get deploy -o jsonpath='{..image}'\` across the estate returns ZERO tag references and only @sha256: digests. Choose ONE substitution point — release.yml writing an image-digest map the GitOps layer consumes, OR Argo kustomize.images — never both." \
  "nothing" \
  "KANZ_TASKS.md OPS-M4a"

mkissue M5 "open-work,P1,tui" \
  "OPS-M4 — TUI release lifecycle: version bump and rollback by digest" \
  "There is nothing to display, propose or restore until deployment consumes digests." \
  "A version bumped from the TUI reaches Running on the new digest, and the previous digest is restored from the TUI and observed Running." \
  "the OPS-M4a digest substitution issue" \
  "KANZ_TASKS.md OPS-M4"

mkissue M5 "open-work,P2,tui" \
  "OPS-M5 — remaining operational surfaces: health, logs, restart, per-service DSNs" \
  "Scoped last deliberately: these are the surfaces that only make sense once the estate can be built, configured and released from the TUI." \
  "Each surface is exercised against the live rig, and one full add-node → configure → deploy → upgrade → roll-back cycle completes without leaving the TUI." \
  "the OPS-M4 release lifecycle issue" \
  "KANZ_TASKS.md OPS-M5"

# ---------------------------------------------------------------- M6 (2)

mkissue M6 "open-work,P1,tui" \
  "Positions, PnL and risk in the TUI from live feeds" \
  "The TradingView overlay is superseded — TV has no market-data redistribution API and its Reuters/Dow Jones news is licensed for display in TV's own UI only. The replacement is the monitoring surface folded into the TUI plus the observability pipeline; between them they deliver what the brief actually asked for without a ToS breach or a parallel spine." \
  "Open positions, PnL and risk metrics are visible in the TUI from live feeds. Constraint: strategy details must NEVER leave the estate — only position and PnL level telemetry is externally visible." \
  "the M1 TUI shell issue" \
  "docs/PLAN.md M6 / docs/GAP.md §0 item 4"

mkissue M6 "open-work,P2,observability" \
  "Grafana or equivalent over the SLO recording rules that already compute" \
  "The recording rules exist and 5 of 6 SLO metrics are present in code; nothing renders them." \
  "A dashboard shows the SLO burn rates the recording rules compute." \
  "the Prometheus deployment issue" \
  "docs/PLAN.md M6"

# ------------------------------------------- verification backlog (6, no milestone)

mkissue none "needs-verification,P0,execution" \
  "Verify the money-arithmetic fixes against a real broker and database" \
  "Four merged fixes, none executed: addDecimal wrapped and was broadly exploitable (6568855); every capital path moved off the wrapping dec.ToProto (6cfc1cc, d11...); the compliance gate's own arithmetic wrapped twice and the second was reachable (ca8049e); the Decimal type had no valid domain, so every consumer assumed one and none enforced it. A wrap bug previously admitted a \$184bn notional against an \$80k book." \
  "Each fix is exercised against a real NATS broker and a real PostgreSQL — not fakeBus, which does not validate envelopes and therefore accepts what a real broker rejects. That gap already let a Critical tenant-context bug ship past a green suite." \
  "nothing" \
  "KANZ_TASKS.md readiness rows, IMPLEMENTED BUT UNVERIFIED"

mkissue none "needs-verification,P0,execution" \
  "Verify the execution-path fixes against a live cluster" \
  "Four merged, unexecuted fixes: COMP-M2 MARKET/STOP valuation and admission (fe5df9b..78f486d, 10 commits); a one-sided book snapshot silently becoming a MARK, the two messages being wire-compatible (34d6cd9); SimVenue MARKET orders silently never filling (2b6db53); cross-USER order ownership, where any Trade holder could cancel or amend ANY order in their tenant (96ca4e4)." \
  "Each is exercised on the kind rig through the full loop, with the observed result recorded." \
  "the SSO portfolios-claim issue gates the cross-user ownership fix reaching production" \
  "KANZ_TASKS.md readiness rows"

mkissue none "needs-verification,P0,security" \
  "Verify the audit-service scoping fixes" \
  "Two merged, unexecuted fixes: the audit read API accepted a caller-supplied ?tenant= with no auth (40bf837), and audit lineage, reports and SOC2 evidence were unscoped while verify was unauthenticated (a5a5be4). Both were green on 158 packages locally, neither has run on a cluster." \
  "A cross-tenant read attempt is refused with the observed status code, and verify rejects an unauthenticated caller." \
  "nothing" \
  "KANZ_TASKS.md readiness rows"

mkissue none "needs-verification,P1,security" \
  "Verify the onboarding and RLS path end-to-end" \
  "ONBOARD-M1…M5 have their refusal paths executed live, but the success paths (200/403, the RLS verdict) are code-checked only. Separately, onboarding verified RLS on 5 tables out of ~15, and the silence is closed but the COVERAGE is not." \
  "The success paths return their expected statuses against a live cluster, and RLS coverage extends to all tenant-scoped tables. Run Postgres-gated tests as a NOSUPERUSER role or RLS is bypassed and the isolation tests pass falsely; and use \`go test -p 1\` or the shared TEST_POSTGRES_URL races." \
  "the ONBOARD-M6 DSN issue determines which database is being checked" \
  "KANZ_TASKS.md readiness rows"

mkissue none "needs-verification,P1,infra" \
  "Verify SPIRE/mTLS and the infra/ manifests against a real cluster" \
  "Compile- and CI-checked only. transport.NewMesh's workload-API call has never run against a real SPIRE agent, and no infra/ manifest has been applied to a cluster carrying the spire-system namespace." \
  "The mesh obtains an SVID from a real SPIRE agent and the manifests apply cleanly." \
  "cluster access — spire-system is absent from the only reachable context" \
  "KANZ_TASKS.md readiness rows"

mkissue none "needs-verification,P1,tui" \
  "Verify the operator control plane and Universe TUI — F0+S1 was never cluster-proven" \
  "Later slices (S2a, S2b, S3a, S3b, S4a) are each VERIFIED COMPLETE through the TUI on a live cluster, but the F0+S1 foundation they sit on — the read-only node and cluster listing — was never itself cluster-proven." \
  "The node and cluster listing panes are exercised against the live rig and their output matches kubectl." \
  "nothing" \
  "KANZ_TASKS.md readiness row, IMPLEMENTED BUT UNVERIFIED — NEVER CLUSTER-PROVEN"

# --------------------------------------- open work, blocked, decisions (9, no milestone)

mkissue none "open-work,P0,infra" \
  "NATS JetStream streams have no durable storage and no re-provisioning path" \
  "Recurred 2026-07-23. Same one-shot-setup-step defect class as the Postgres role that a pod restart destroyed." \
  "Streams survive a broker restart, and a re-provisioning path exists that does not depend on a hand-run command." \
  "nothing" \
  "KANZ_TASKS.md readiness row, OPEN WORK"

mkissue none "open-work,P1,dr" \
  "nats-rebuild has no tenant dimension — tenant-prefixed topics are outside DR" \
  "The scope boundary is documented; the capability is absent. Tenant-prefixed topics would not be restored by a spine rebuild." \
  "A rebuild restores tenant-prefixed topics, asserted against a rig carrying more than one tenant prefix." \
  "nothing" \
  "KANZ_TASKS.md readiness row, OPEN WORK"

mkissue none "open-work,P2,execution" \
  "dec.ToProto still wraps for ~30 non-capital callers" \
  "Deliberate, guarded and low priority: reporting and analytics callers keep the wrapping conversion. Every capital path is already off it." \
  "Either the remaining callers move off the wrapping conversion, or a guard asserts that no capital path can reach it." \
  "nothing" \
  "KANZ_TASKS.md readiness row, OPEN WORK (deliberate)"

mkissue none "open-work,P1,execution" \
  "Bus-wide Decimal validation — one ingress is bounded, the generic case is not" \
  "The refusal contract narrowed one ingress. The generic bus path still accepts a Decimal outside the valid domain." \
  "A Decimal outside the valid domain is refused at the bus boundary, proven against a real broker." \
  "nothing" \
  "KANZ_TASKS.md readiness row, OPEN WORK (narrowed, not closed)"

mkissue none "open-work,P2,execution" \
  "The OMS market.> subscription is narrowed but its volume is still unbounded" \
  "34d6cd9 narrowed the subscription; COMP-M2's price fold still consumes an unbounded stream." \
  "The subscription volume is bounded by construction, not by current market conditions." \
  "nothing" \
  "KANZ_TASKS.md readiness row, PARTIALLY FIXED"

mkissue none "decision-needed,P1,infra" \
  "Multi-tenant runtime — provisioning creates no compute" \
  "oms/main.go:349 constructs NewTenantPool(ctx, DSN, cfg...), but tenant provisioning creates no compute for the tenant it provisions. This is an architectural decision, not a coding task." \
  "A written ruling on whether tenants get dedicated compute or share a pool, recorded in KANZ_BRAIN.md." \
  "an owner ruling" \
  "KANZ_TASKS.md readiness row, ARCHITECTURAL DECISION REQUIRED"

mkissue none "blocked-external,P0,infra" \
  "Does a production environment exist at all?" \
  "One kubectl context has ever been reachable (kind-kanz-dryrun). The kanz-data, spire-system and vault namespaces are absent from it. Every deploy-shaped acceptance criterion in the milestone plan assumes an environment nobody here has seen. If the only estate is a kind rig, the DR and node-management milestones change shape." \
  "Someone with access states which environments exist and how to reach them, recorded in KANZ_BRAIN.md. Answering the wrong case produces a confident, wrong architecture record — establish which case you are in FIRST." \
  "access outside this box" \
  "KANZ_TASKS.md readiness row / docs/PLAN.md risk 1"

mkissue none "blocked-external,P0,security" \
  "Eighred SSO must issue the portfolios claim before 96ca4e4 reaches production" \
  "The cross-USER order-ownership fix is merged and depends on a portfolios claim in the token. This is a lead action, not code: without the claim, the fix either refuses legitimate callers or is inert." \
  "Eighred SSO issues the portfolios claim, and a token carrying it is observed passing the ownership check." \
  "an SSO change outside this repository" \
  "KANZ_TASKS.md readiness row, BLOCKING PREREQUISITE"

mkissue none "open-work,P2,execution" \
  "Locate the venue build tags — the documented location is wrong" \
  "KANZ_BRAIN.md states the websocket links only under //go:build binance / okx, verified via go list -deps. A search of services/venue-*/internal/*/*.go found no go:build hits at that depth. The tags are elsewhere in the tree and have not been pinned. A stale claim about where a control lives is the same failure class as a guard that does not guard." \
  "The tagged files are named, and either KANZ_BRAIN.md is corrected or a guard asserts the vendor-free default build via go list -deps." \
  "nothing" \
  "docs/ANALYSIS.md §7 and open question 3 — affects the M3 rollback story"

# ------------------------------------- unscheduled, credential-gated (9, no milestone)

mkissue none "blocked-external,P2,schemas" \
  "PARITY-07b — publish target: a tagged kanz-schemas-go companion repo and a private GOPROXY" \
  "The workflow and CI guard already exist (PARITY-07a); only the publish target is missing." \
  "A tagged internal kanz-schemas-go repo and a private GOPROXY exist, plus a private index or GitHub Packages for the Python and TypeScript packages." \
  "infrastructure provisioning" \
  "KANZ_TASKS.md PARITY-07b"

mkissue none "blocked-external,P2,schemas" \
  "PARITY-07c — run the SDK release pipeline to tagged Go, Python and TypeScript artifacts" \
  "The pipeline exists and has never had a target to publish to." \
  "The kanz-schemas CI produces a tagged kanz-schemas-go plus Python and TypeScript SDK artifacts." \
  "the PARITY-07b publish-target issue" \
  "KANZ_TASKS.md PARITY-07c"

mkissue none "decision-needed,P2,schemas" \
  "PARITY-07d — pin published versions, delete the replace directives, arm the dormant guard" \
  "kanz/go.mod carries a load-bearing \`replace github.com/kanz-eng/kanz-schemas-go => ../kanz-schemas/gen/go\`, which is why every Dockerfile builds from the repo root rather than kanz/. BLOCKED ON A MODULE-PATH DECISION: the module is github.com/kanz-eng/kanz but the repository is eighred/kanz. Harmless under replace; fatal once a version has to resolve." \
  "The replace directives are gone from kanz/go.mod and the kanz-py/ts equivalents, the KANZ_SDK_PUBLISHED no-replace CI guard is armed, and the module path resolves." \
  "the PARITY-07c pipeline issue, and a ruling on the module path" \
  "KANZ_TASKS.md PARITY-07d"

mkissue none "blocked-external,P2,infra" \
  "Composition-root vendor Source bindings: Bloomberg, Refinitiv, ICE, anthropic, go-redis" \
  "Each sits behind a delivered seam; only the external boundary is missing. Note the standing rule: a build tag CI does not compile is not a feature, it is dead code with a plan attached." \
  "Each binding is wired in its composition root and CI compiles the tag." \
  "vendor SDKs and credentials" \
  "KANZ_TASKS.md credential-gated carried-forward"

mkissue none "blocked-external,P2,compliance" \
  "Licensed datasets for calibration and filing reconciliation" \
  "ISDA SIMM, BCBS FRTB and NGFS climate parameter tables, the live FX feed, and the regulator worked-example datasets load into delivered *Inputs and parameter seams." \
  "The datasets load and reconcile against the regulators' worked examples." \
  "dataset licensing" \
  "KANZ_TASKS.md credential-gated carried-forward"

mkissue none "blocked-external,P2,infra" \
  "Real infra and human loops: live Redis, a real DR region, the PITR drill, SOC 2, first-client UAT" \
  "Redis is a single instance today; EXEC-M22 removed the planned outage and converted the unplanned one into a fail-closed path. The platform has NEVER executed a disaster-recovery failover." \
  "A live Redis and a real DR region exist, the migrations are applied against kanz-books, the PITR drill runs for real, the third-party SOC 2 engagement starts, and first-client UAT executes." \
  "infrastructure and external parties" \
  "KANZ_TASKS.md credential-gated carried-forward"

mkissue none "open-work,P2,execution" \
  "SOV-03 — sovereign telemetry: cross-tenant NAV, growth, fee and slippage" \
  "DEFERRED by lead decision 2026-07-16 until a second tenant exists: the estate has exactly one tenant, so a cross-tenant view has nothing to cross. The ledger IS bitemporal, so the inputs exist." \
  "A cross-tenant NAV, growth, fee and slippage view for the operator of the whole estate." \
  "a second tenant existing" \
  "KANZ_TASKS.md SOV-03 / KANZ_BRAIN.md"

mkissue none "open-work,P2,security" \
  "SOV-04 — license revocation, built on the kanz-halt primitive" \
  "Ground truth: does not exist. No license, no WipeNodeConfiguration RPC. The primitive to build on is kanz-halt. NOTE THE SUPERSEDED HALF: RAM and binary wiping was a host-vector design that did not survive the 2026-07-16 Kubernetes decision. On k8s, revocation means stopping the tenant's workloads, purging its Vault-CSI secrets and scaling to zero, with kanz-halt as the in-band brake that stops trading first." \
  "A tenant's workloads stop, its Vault-CSI secrets are purged, and it scales to zero — with trading halted first." \
  "nothing" \
  "KANZ_TASKS.md SOV-04 / docs/GAP.md §0 item 2"

mkissue none "open-work,P2,execution" \
  "SOV-06 — push telemetry back to TradingView" \
  "The chart surface is PULL: authorized clients poll /v1/broker/accounts/{id}/… through the gateway (EXEC-M15)." \
  "Outbound telemetry carries instrument, side, size and entry, and NOTHING about how the decision was made. Strategy details must never leave the estate." \
  "nothing" \
  "KANZ_TASKS.md SOV-06"

echo
echo "total rendered/created this run: $COUNT"
```

- [ ] **Step 2: Make it executable and dry-run the whole board**

```bash
chmod +x "$SCRATCH/create-issues.sh"
"$SCRATCH/create-issues.sh" --dry-run | tail -3
```

Expected final line: `total rendered/created this run: 55`

If it is not 55, a `mkissue` call is malformed — fix it before going further. Do not proceed with a different number "because it looks close".

- [ ] **Step 3: Verify the milestone distribution**

```bash
for m in M0 M1 M2 M3 M4 M5 M6 none; do
  printf '%-5s %s\n' "$m" "$("$SCRATCH/create-issues.sh" --dry-run "$m" | grep -c '^TITLE: ')"
done
```

Expected exactly:

```
M0    10
M1    3
M2    2
M3    3
M4    2
M5    9
M6    2
none  24
```

- [ ] **Step 4: Eyeball one rendered body**

```bash
"$SCRATCH/create-issues.sh" --dry-run M0 | head -12
```

Expected: a `TITLE:` line followed by `**Evidence:**`, `**Verified when:**`, `**Blocked by:**` and `**Source:**` paragraphs — all four populated, none empty.

No commit — the script is scratchpad-only and deliberately not repo tooling.

---

### Task 3: Create the M0 issues

Ten issues first, so the body format is gated by a reviewer before the remaining 45 are created in the same shape.

**Files:**
- Modify: `$SCRATCH/created.log`

**Interfaces:**
- Consumes: `create-issues.sh` and the Task 1 labels/milestones.
- Produces: 10 GitHub issues and 10 lines in `created.log`. Task 4 reads `created.log` to skip them.

- [ ] **Step 1: Create them**

```bash
"$SCRATCH/create-issues.sh" M0
```

Expected: ten `created #N  <title>` lines, then `total rendered/created this run: 10`.

- [ ] **Step 2: Verify the count and the milestone**

```bash
gh issue list --repo eighred/kanz --milestone M0 --state open --json number --jq 'length'
wc -l < "$SCRATCH/created.log"
```

Expected: `10` and `10`.

- [ ] **Step 3: Read one issue back in full**

```bash
gh issue view "$(head -1 "$SCRATCH/created.log" | cut -f1)" --repo eighred/kanz
```

Expected: labels `open-work`, `P1`, `ci`; milestone `M0`; a body carrying all four fields with the backticked shell command intact and unmangled.

- [ ] **Step 4: Prove the idempotency guard works**

This is the safety net for a partial failure. It must be exercised, not assumed.

```bash
"$SCRATCH/create-issues.sh" M0 | tail -3
```

Expected: ten `SKIP (already created):` lines and `total rendered/created this run: 0`. **No new issues.**

```bash
gh issue list --repo eighred/kanz --milestone M0 --state open --json number --jq 'length'
```

Expected: still `10`.

No commit — no repository files changed.

---

### Task 4: Create the remaining 45 issues

**Files:**
- Modify: `$SCRATCH/created.log`

**Interfaces:**
- Consumes: `create-issues.sh`, `created.log` (to skip M0).
- Produces: 55 total issues. Task 12's PR body references the count.

- [ ] **Step 1: Create everything not already logged**

```bash
"$SCRATCH/create-issues.sh" | tail -3
```

Expected: `total rendered/created this run: 45` (the ten M0 titles are skipped).

- [ ] **Step 2: Verify the totals**

```bash
gh issue list --repo eighred/kanz --state open --limit 100 --json number --jq 'length'
wc -l < "$SCRATCH/created.log"
```

Expected: `55` and `55`.

- [ ] **Step 3: Verify every P0 in GAP.md §2 has an issue**

Checked by name, not by count — a matching count with the wrong items is the failure this step exists to catch.

```bash
gh issue list --repo eighred/kanz --state open --limit 100 --json title --jq '.[].title' > "$SCRATCH/titles.txt"
grep -ci 'cosign verify'                "$SCRATCH/titles.txt"   # C6
grep -ci 'CNPG cluster or a named'      "$SCRATCH/titles.txt"   # C1
grep -ci 'Deploy a Prometheus'          "$SCRATCH/titles.txt"   # C2
grep -ci 'data-quality.rules.yaml'      "$SCRATCH/titles.txt"   # C3
grep -ci 'secret() helper'              "$SCRATCH/titles.txt"   # C4
grep -ci 'Docker Hub SPOF'              "$SCRATCH/titles.txt"   # C5
```

Expected: `1` six times.

- [ ] **Step 4: Verify every issue carries exactly one kind label and one priority label**

```bash
gh issue list --repo eighred/kanz --state open --limit 100 --json number,labels \
  --jq '.[] | select(([.labels[].name] | map(select(. == "open-work" or . == "needs-verification" or . == "blocked-external" or . == "decision-needed")) | length) != 1) | .number'
gh issue list --repo eighred/kanz --state open --limit 100 --json number,labels \
  --jq '.[] | select(([.labels[].name] | map(select(. == "P0" or . == "P1" or . == "P2")) | length) != 1) | .number'
```

Expected: no output from either command.

- [ ] **Step 5: Snapshot the board for the PR body**

```bash
gh issue list --repo eighred/kanz --state open --limit 100 --json number,title,milestone,labels \
  --jq '.[] | "#\(.number)\t\(.milestone.title // "backlog")\t\(.title)"' | sort -k2 > "$SCRATCH/board-snapshot.txt"
wc -l < "$SCRATCH/board-snapshot.txt"
```

Expected: `55`.

No commit — no repository files changed.

---

### Task 5: Consolidate the duplicate superpowers tree

Twelve plans and specs live under `kanz/docs/superpowers/` instead of `docs/superpowers/`. Two locations for one thing is the duplication this whole change exists to remove.

**Files:**
- Move: `kanz/docs/superpowers/plans/*.md` (6) → `docs/superpowers/plans/`
- Move: `kanz/docs/superpowers/specs/*.md` (6) → `docs/superpowers/specs/`

**Interfaces:**
- Consumes: nothing.
- Produces: a single plans/specs location. No later task depends on it.

- [ ] **Step 1: Confirm no filename collisions before moving**

The dated filenames should be disjoint, but a collision would silently overwrite a plan.

```bash
comm -12 \
  <(ls kanz/docs/superpowers/plans kanz/docs/superpowers/specs | sort) \
  <(ls docs/superpowers/plans docs/superpowers/specs | sort)
```

Expected: no output.

- [ ] **Step 2: Move them with git**

```bash
git mv kanz/docs/superpowers/plans/*.md docs/superpowers/plans/
git mv kanz/docs/superpowers/specs/*.md docs/superpowers/specs/
rmdir kanz/docs/superpowers/plans kanz/docs/superpowers/specs kanz/docs/superpowers
```

- [ ] **Step 3: Verify**

```bash
git ls-files kanz/docs/superpowers | wc -l
git status --porcelain | grep -c '^R'
```

Expected: `0` and `12`.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "$(cat <<'EOF'
docs: consolidate the duplicate superpowers tree into docs/

Twelve plans and specs lived under kanz/docs/superpowers/ while the rest
lived under docs/superpowers/. Two locations for one thing.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Move the superseded directions into KANZ_BRAIN.md

The five items in `docs/GAP.md` §0 are the highest-value content in the documents being deleted: they record what was decided *against*. An anti-decision is not work, so it belongs in architectural memory, not on the board.

**Files:**
- Modify: `KANZ_BRAIN.md` (the `## Anti-decisions` section, which begins at line 134)

**Interfaces:**
- Consumes: nothing.
- Produces: three new anti-decision bullets. Task 7 deletes the documents that were their only other home, so this must land first.

- [ ] **Step 1: Confirm which of the five are already recorded**

Three of the five are already in `KANZ_BRAIN.md` and must NOT be duplicated — duplication is the failure this change exists to remove.

```bash
grep -c 'THERE IS NO TENANT-MASTER TIER IN KANZ' KANZ_BRAIN.md
grep -c 'THE ESTATE IS KUBERNETES-MANAGED' KANZ_BRAIN.md
grep -c 'TWO IDENTITY PLANES' KANZ_BRAIN.md
```

Expected: `1`, `1`, `1`. These cover §0 items 3 (`root@company`), 2's host-vector premise, and 5 (SSH-fingerprint registration). Only items 1, 2's explicit wipe semantics, and 4 need adding.

- [ ] **Step 2: Add the three missing anti-decisions**

Insert these three bullets immediately **before** the final line of the `## Anti-decisions` section (the line beginning `- No committed generated code;`), keeping that summary line last:

```markdown
- **NODE TEARDOWN FROM THE TUI IS NOT AN OPERATOR-RUNTIME OPERATION (2026-07-27).** Decommissioning a node is a provisioning/administrative act, not something a long-running operator should be able to do at any moment. `operator-node-writer` grants `nodes:[patch]` and `pods/eviction:[create]` only; `operator_rbac_test.go:96` forbids `create`, `delete`, `deletecollection`, `update` and `*`. Granting the operator standing node-delete was explicitly rejected — the blast radius of a compromised long-running operator is the whole estate. Cordon, drain and relabel are the supported lifecycle; removal happens through the provisioning plane.
- **THE KILL-SWITCH DOES NOT WIPE RAM, BINARIES OR DB CONNECTION STATE (2026-07-27).** That was a host-vector design and it did not survive the 2026-07-16 Kubernetes decision — there is no host to wipe when the unit of deployment is a pod. On k8s, revocation is: stop the tenant's workloads, purge its Vault-CSI secrets, scale to zero, with `kanz-halt` as the in-band brake that stops trading FIRST. Anyone re-proposing wipe semantics is describing the cancelled SSH estate.
- **TRADINGVIEW IS AN OUTBOUND AND WEBHOOK BOUNDARY ONLY — NEVER A MARKET-DATA SOURCE (2026-07-27).** TV has no market-data redistribution API, and its Reuters/Dow Jones news is licensed for display in TV's own UI only; consuming either breaches ToS and the underlying exchange licensing. Pine → webhook is the one supported direction and is already built (`services/webhook-ingest`, HMAC + nonce replay). `tv-sync` is the OPPOSITE end of the loop — the outbound Broker-API projection — not an ingest node. The monitoring surface the overlay was meant to provide is delivered by the TUI plus the observability pipeline instead.
```

- [ ] **Step 3: Verify all five §0 items are now recorded and the section is intact**

```bash
grep -c 'NODE TEARDOWN FROM THE TUI'        KANZ_BRAIN.md
grep -c 'DOES NOT WIPE RAM'                 KANZ_BRAIN.md
grep -c 'NEVER A MARKET-DATA SOURCE'        KANZ_BRAIN.md
grep -c '^## Anti-decisions'                KANZ_BRAIN.md
tail -n +134 KANZ_BRAIN.md | grep -n 'No committed generated code' | head -1
```

Expected: `1`, `1`, `1`, `1`, and the `No committed generated code` line still present as the last bullet of the section.

- [ ] **Step 4: Commit**

```bash
git add KANZ_BRAIN.md
git commit -m "$(cat <<'EOF'
docs(brain): record the three unrecorded superseded directions

GAP.md section 0 listed five decided-against directions. Three were
already in KANZ_BRAIN.md; the other three - TUI node teardown, kill-switch
wipe semantics, and the TradingView overlay - existed only in a document
about to be deleted. An anti-decision is not work, so it belongs in
architectural memory rather than on the board.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Delete the superseded tracking documents

**Files:**
- Delete: `KANZ_TASKS.md`, `KANZ_ROADMAP.md`, `DEMO_DEPLOYMENT.md`, `docs/RELEASE_READINESS.md`, `tools/validate-board.sh`

**Interfaces:**
- Consumes: Tasks 3, 4 and 6 — every open item is an issue and every anti-decision is in `KANZ_BRAIN.md` before anything is removed.
- Produces: nothing. Task 8 rewrites `CLAUDE.md`, which references these paths.

- [ ] **Step 1: Confirm the board exists as issues before deleting its source**

Guard against deleting the documents on a run where issue creation silently failed.

```bash
gh issue list --repo eighred/kanz --state open --limit 100 --json number --jq 'length'
```

Expected: `55`. **If this is not 55, STOP.** Do not delete.

- [ ] **Step 2: Delete**

```bash
git rm KANZ_TASKS.md KANZ_ROADMAP.md DEMO_DEPLOYMENT.md docs/RELEASE_READINESS.md tools/validate-board.sh
```

- [ ] **Step 3: Confirm nothing still references the deleted paths**

`CLAUDE.md` is expected here — Task 8 rewrites it. Any *other* hit is a dangling reference that must be fixed now.

```bash
grep -rl --exclude-dir=.git --exclude-dir=node_modules \
  -e 'KANZ_TASKS' -e 'KANZ_ROADMAP' -e 'DEMO_DEPLOYMENT' -e 'RELEASE_READINESS' -e 'validate-board' . \
  | grep -v '^./docs/superpowers/'
```

Expected: `./CLAUDE.md` and nothing else. Historical plans and specs under `docs/superpowers/` legitimately reference the old board and are excluded — they are dated records, not live pointers.

- [ ] **Step 4: Commit**

```bash
git commit -m "$(cat <<'EOF'
docs: delete the superseded tracking documents

Open work now lives in GitHub Issues (55 issues, milestones M0-M6).
KANZ_TASKS.md was 355 KB with a 155 KB DONE section; KANZ_ROADMAP.md and
the PATH TO PARITY section were a third and fourth roadmap. Git history
and claude-mem hold the completed record. validate-board.sh validated a
file that no longer exists.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: Rewrite CLAUDE.md

It currently reproduces the entire `/forge-tasks` engineering specification, which already exists as the `kanz-forge:engineering-standard` skill. A project's `CLAUDE.md` should name where things live and stop.

**Files:**
- Modify: `CLAUDE.md` (full replacement)

**Interfaces:**
- Consumes: Task 7's deletions.
- Produces: the in-repo statement of the four layers. Task 10 makes the session hook agree with it.

- [ ] **Step 1: Replace the file entirely with this content**

```markdown
# kanz

The Eighred institutional trading and risk platform. Go monorepo under `kanz/`,
Protobuf contracts under `kanz-schemas/`, the Python inference service under
`kanz-py/`.

## Where things live

Four layers. Nothing is tracked in two places.

| Layer | Home |
|---|---|
| **What to do** | **GitHub Issues** on `eighred/kanz` — milestones `M0`…`M6` carry the sequence |
| **Why it is shaped this way** | `KANZ_BRAIN.md` — durable architectural decisions and anti-decisions |
| **What happened** | git history and claude-mem |
| **Specs and plans** | `docs/superpowers/{specs,plans}/` |

There is no task board in this repository. `KANZ_TASKS.md`, `KANZ_ROADMAP.md` and
the Phase 0/2/3 documents were deleted on 2026-07-28; five documents each claimed
to say what to do next and none deferred to the others.

## Issue conventions

Every issue carries exactly one **kind** label — `open-work`,
`needs-verification`, `blocked-external`, `decision-needed` — one **priority**
(`P0`/`P1`/`P2`), and an **area**.

`needs-verification` means *the code is merged and nothing has run it*. It is a
distinct state from done, and it is tracked because this repository has repeatedly
found that a merged fix, a green suite and a written claim are not evidence.

An issue body states four things: **Evidence** (a `file:line`, a commit, or a
quoted command result), **Verified when** (a runnable command and its expected
result), **Blocked by**, and **Source**. An issue that cannot say how it will be
proven does not belong on the board.

## The engineering bar

The standard for what is worth building, and what "good" means here, is the
`kanz-forge:engineering-standard` skill. It is not restated in this file.

## Sequencing constraints that outlive any one issue

- **M0 first, and not partially.** Everything after it is verified by a pipeline
  that cannot presently complete a merge unaided.
- **M3 must not precede the DR-coverage issue in M0.** Placing real orders against
  a store that may not be backed up is the one ordering error with an
  unrecoverable failure mode.

## Working notes

- Go tests race on a shared `TEST_POSTGRES_URL` — run `go test -p 1 ./...`.
- Postgres-gated tests skip silently without `TEST_POSTGRES_URL`, and must run as
  a **NOSUPERUSER** role or RLS is bypassed and the isolation tests pass falsely.
- `fakeBus` does not validate envelopes, so it accepts what a real broker rejects.
  A green suite using it is not a broker proof.
```

- [ ] **Step 2: Verify it no longer restates the engineering standard**

```bash
grep -c 'Production readiness is the only metric' CLAUDE.md
grep -c 'GitHub Issues' CLAUDE.md
wc -l < CLAUDE.md
```

Expected: `0`, at least `1`, and a line count under 60 (it was 300+).

- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md
git commit -m "$(cat <<'EOF'
docs: rewrite CLAUDE.md as a pointer to the four layers

It reproduced the whole /forge-tasks engineering specification, which
already exists as the kanz-forge:engineering-standard skill. It now names
where work, decisions, history and plans live, plus the conventions and
sequencing constraints that outlive any single issue.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 9: Clear the agent scratch and reduce the memory injection

> **RUNS LAST — after Task 12.** (Owner decision, 2026-07-28, from the pre-flight
> scan.) `.superpowers/sdd/` is where this plan's own task briefs, reports, review
> diffs and progress ledger are written. Deleting it at position 9 would destroy
> Tasks 10–12's inputs mid-execution. Execution order is
> **1 → 8, 10, 11, 12, then 9.**

**Files:**
- Delete: `.superpowers/sdd/**` (236 files, gitignored)
- Modify: `~/.claude-mem/settings.json`

**Interfaces:**
- Consumes: nothing.
- Produces: nothing. Independent of every other task.

- [ ] **Step 1: Confirm the directory is fully gitignored before deleting**

This deletion is **not** recoverable from git. Prove nothing tracked lives there.

```bash
git ls-files .superpowers | wc -l
ls .superpowers/sdd | wc -l
```

Expected: `0` tracked files. The second number is **at least 236** — this task
runs last, so it also sees this plan's own briefs, reports and review diffs.
`git ls-files .superpowers` returning `0` is the safety gate; the file count is
informational and must NOT be asserted as an exact value.

- [ ] **Step 2: Delete the scratch, keeping the .gitignore**

```bash
find .superpowers/sdd -type f ! -name '.gitignore' -delete
ls -a .superpowers/sdd
```

Expected: only `.`, `..` and `.gitignore`.

- [ ] **Step 3: Reduce the session-start injection**

Edit `~/.claude-mem/settings.json`, changing only this line:

```json
  "CLAUDE_MEM_CONTEXT_OBSERVATIONS": "20",
```

- [ ] **Step 4: Verify the JSON is still valid and the DB is untouched**

An invalid settings file silently disables the memory layer that is now the record of everything the deleted `DONE` section held.

```bash
node -e "const s=require(require('os').homedir()+'/.claude-mem/settings.json'); console.log(s.CLAUDE_MEM_CONTEXT_OBSERVATIONS)"
ls -la ~/.claude-mem/claude-mem.db | awk '{print $5}'
```

Expected: `20`, and a file size of roughly 36,855,808 bytes — unchanged. **The DB is never edited by hand.**

No commit — nothing tracked changed.

---

### Task 10: Update the kanz-forge routing so future sessions find the board

Without this, every future session is told to consult `KANZ_TASKS.md`, which no longer exists. The hook file's own comment requires it to stay consistent with the skill, so both change together.

**Files:**
- Modify: `~/.claude/skills/kanz-forge/hooks/session-start.js:33-36`
- Modify: `~/.claude/skills/kanz-forge/skills/stack-routing/SKILL.md` (the *Where memory lives* section)

**Interfaces:**
- Consumes: Task 8's `CLAUDE.md`, which these must agree with.
- Produces: a routing block that names GitHub Issues for this repository.

- [ ] **Step 1: Edit the hook**

In `session-start.js`, replace this paragraph:

```
Never create a new *_TASKS.md / *_BRAIN.md board. The four legacy boards
(eighred-kanz, eighred-SSO, kanz-web, originbond) hold open work only and never
outrank the code. Plans live where superpowers puts them, history lives in
claude-mem, cross-project facts live in native memory.
```

with:

```
Never create a new *_TASKS.md / *_BRAIN.md board. eighred-kanz tracks open work
in GITHUB ISSUES (eighred/kanz, milestones M0-M6) and keeps KANZ_BRAIN.md for
architectural decisions only - it has no task board. The three remaining legacy
boards (eighred-SSO, kanz-web, originbond) hold open work only and never outrank
the code. Plans live where superpowers puts them, history lives in claude-mem,
cross-project facts live in native memory.
```

- [ ] **Step 2: Verify the hook still runs and emits the new text**

A hook that throws is a hook that silently stops routing.

```bash
node ~/.claude/skills/kanz-forge/hooks/session-start.js | grep -c 'GITHUB ISSUES'
node ~/.claude/skills/kanz-forge/hooks/session-start.js | grep -c 'KANZ_TASKS.md'
```

Expected: `1` and `0`.

- [ ] **Step 3: Make the skill agree**

In `stack-routing/SKILL.md`, under *Where memory lives*, replace the sentence beginning `**Legacy boards that already exist** (\`eighred-kanz\`, ...` so the list reads `(\`eighred-SSO\`, \`kanz-web\`, \`originbond\`)`, and add this paragraph immediately before it:

```markdown
**`eighred-kanz` has no board.** Its open work lives in **GitHub Issues** on
`eighred/kanz`, with milestones `M0`–`M6` carrying the sequence and one `kind`
label per issue (`open-work`, `needs-verification`, `blocked-external`,
`decision-needed`). `KANZ_TASKS.md` and `KANZ_ROADMAP.md` were deleted on
2026-07-28; `KANZ_BRAIN.md` remains, for architectural decisions only. Read the
board with `gh issue list --repo eighred/kanz`, never from a file.
```

- [ ] **Step 4: Verify the two agree**

```bash
grep -c 'eighred-kanz` has no board' ~/.claude/skills/kanz-forge/skills/stack-routing/SKILL.md
grep -c 'eighred-kanz' ~/.claude/skills/kanz-forge/skills/stack-routing/SKILL.md
```

Expected: `1`, and a total `eighred-kanz` count of `2` — the new paragraph and nothing in the legacy-board list.

No commit — these files are outside the repository.

---

### Task 11: Correct the stale branch-workflow memory

Native memory instructs "commit directly to `main`, never open PRs". The repository now runs PRs behind an active branch-protection rule, which blocked PR #46 on 2026-07-28. A stale instruction that contradicts observable practice is worse than no instruction.

**Files:**
- Modify: `~/.claude/projects/C--Users-root-Desktop-eighred-kanz/memory/kanz-main-only-workflow.md`
- Modify: `~/.claude/projects/C--Users-root-Desktop-eighred-kanz/memory/MEMORY.md` (its index line)

**Interfaces:**
- Consumes: nothing.
- Produces: nothing.

- [ ] **Step 1: Confirm branch protection is actually active before rewriting**

Do not correct a memory on the strength of one recollection.

```bash
gh api repos/eighred/kanz/branches/main/protection --jq '.required_pull_request_reviews != null or .required_status_checks != null'
```

Expected: `true`. If this errors or returns `false`, STOP and report — the original memory may still be correct and the rewrite would be wrong.

- [ ] **Step 2: Replace the memory body**

Keep the frontmatter `name` and `metadata` unchanged so the index line still resolves. Replace the `description` and the body:

```markdown
---
name: kanz-main-only-workflow
description: "In eighred-kanz, work lands on a branch behind a PR — main is branch-protected; the 2026-07-14 commit-straight-to-main rule no longer applies"
metadata:
  node_type: memory
  type: feedback
  originSessionId: 0e9d2317-feb9-4248-ac24-ca4306c1bd54
---

Work on a branch and land it through a PR in `eighred/kanz`. `main` is
branch-protected: a merge was blocked on PR #46 on 2026-07-28 for failing to
satisfy a protection rule, so committing straight to `main` is not merely
discouraged, it is rejected.

**This supersedes the 2026-07-14 instruction** recorded here previously
("commit directly to main, never open PRs"). That was true when the user was
the sole engineer and `main` had just absorbed a 155-commit divergence. It
stopped being true once protection was enabled and the repository began running
PRs #45 onward. The underlying concern — one linear history, no second place
the truth can live — is now served by short-lived branches that merge and are
deleted, not by avoiding branches.

**How to apply:** branch, commit, push, open a PR, let the checks run. Delete
the branch after merge. The remote is `origin` → `https://github.com/eighred/kanz`
(private, in the `eighred` org).

Note the Go module path is `github.com/kanz-eng/kanz`, which does NOT match the
repository path. Harmless for the monorepo, which is consumed via `replace`, but
it must be settled before PARITY-07d publishes a versioned SDK under a path that
has to resolve. That is tracked as a `decision-needed` issue on the board.
```

- [ ] **Step 3: Update the index line**

In `MEMORY.md`, replace the `kanz-main-only-workflow` line with:

```markdown
- [kanz-main-only-workflow](kanz-main-only-workflow.md) — branch + PR in eighred-kanz; main is branch-protected, superseding the old commit-straight-to-main rule
```

- [ ] **Step 4: Verify**

```bash
MEM=~/.claude/projects/C--Users-root-Desktop-eighred-kanz/memory
grep -c 'never open PRs' "$MEM/kanz-main-only-workflow.md"
grep -c 'branch-protected' "$MEM/MEMORY.md"
```

Expected: `0` and `1`.

No commit — these files are outside the repository.

---

### Task 12: Open the PR and close the superseded documentation PRs

**Files:**
- No file changes. GitHub state only.

**Interfaces:**
- Consumes: every prior task.
- Produces: one open PR. **Do not merge it.**

- [ ] **Step 1: Run the repository's own build as a regression check**

This change touches no Go code, so a failure here is pre-existing. Report it as pre-existing; do not fix it in this branch and do not absorb it into the PR description as if it were caused here.

```bash
cd kanz && go build ./... && go vet ./... ; cd ..
```

Expected: clean. If not, capture the output verbatim for the report.

- [ ] **Step 2: Push the branch**

```bash
git push -u origin chore/tracking-consolidation
```

- [ ] **Step 3: Open the PR**

```bash
gh pr create --repo eighred/kanz --base main --head chore/tracking-consolidation \
  --title "chore: consolidate tracking into GitHub Issues" \
  --body "$(cat <<'EOF'
Five documents each claimed to say what to do next and none deferred to the
others: `KANZ_TASKS.md` TODO, `KANZ_TASKS.md` PATH TO PARITY, `KANZ_ROADMAP.md`,
`docs/PLAN.md` and `docs/GAP.md`.

## Four layers, no fifth

| Layer | Home |
|---|---|
| What to do | **GitHub Issues** — 55 issues, milestones M0–M6 |
| Why it is shaped this way | `KANZ_BRAIN.md` |
| What happened | git history and claude-mem |
| Cross-project facts | native memory |

## Deleted

`KANZ_TASKS.md` (355 KB, of which a 155 KB `DONE` section), `KANZ_ROADMAP.md`,
`DEMO_DEPLOYMENT.md`, `docs/RELEASE_READINESS.md`, and `tools/validate-board.sh`
— which validated a file that no longer exists. The duplicate
`kanz/docs/superpowers/` tree (12 files) moved into `docs/superpowers/`.

## Preserved deliberately

- Every open item became an issue carrying its evidence and a runnable
  acceptance command.
- `needs-verification` survives as a label. It means *the code is merged and
  nothing has run it* — the readiness board's founding premise, and the one idea
  in the old board worth carrying forward.
- The five superseded directions from `GAP.md` §0 became anti-decisions in
  `KANZ_BRAIN.md`. Three were already recorded there and were not duplicated.

## Supersedes

Closes PRs #50, #51, #52, #53 and #54, which add or edit the deleted files.
`docs/PLAN.md`, `docs/GAP.md` and `docs/ANALYSIS.md` never reached `main`; their
findings are on the board. PR #49 is a code fix and is unaffected.

No Go code is touched.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

- [ ] **Step 4: Close the five superseded documentation PRs**

```bash
PR_URL=$(gh pr view chore/tracking-consolidation --repo eighred/kanz --json url --jq .url)
for n in 50 51 52 53 54; do
  gh pr close "$n" --repo eighred/kanz --delete-branch \
    --comment "Superseded by ${PR_URL}: open work moved to GitHub Issues (55 issues, milestones M0-M6) and the documents this PR touches were deleted. This PR's findings are on the board; nothing is lost."
done
```

- [ ] **Step 5: Final verification — the whole spec's acceptance list**

```bash
gh pr list --repo eighred/kanz --state open --json number --jq '.[].number' | sort -n | tr '\n' ' '
gh issue list --repo eighred/kanz --state open --limit 100 --json number --jq 'length'
git ls-files | grep -cE '(KANZ_TASKS|KANZ_ROADMAP|DEMO_DEPLOYMENT|RELEASE_READINESS|validate-board)'
git ls-files kanz/docs/superpowers | wc -l
grep -c '^## Anti-decisions' KANZ_BRAIN.md
```

Expected, in order: `49 <new-pr-number>` and nothing else; `55`; `0`; `0`; `1`.

---

## Self-review

**Spec coverage.** Every section of the spec maps to a task: the taxonomy and body contract → Tasks 1–2; the 55-issue inventory → Tasks 2–4; the `kanz/docs/superpowers` duplicate → Task 5; the §0 anti-decisions → Task 6; the deletions → Task 7; the `CLAUDE.md` rewrite → Task 8; the scratch clear and the claude-mem setting → Task 9; the two `kanz-forge` files → Task 10; the native-memory correction → Task 11; the PR and the five closures → Task 12. The spec's eight verification commands appear across Tasks 4, 7, 9, 10 and 12, with the full list re-run in Task 12 Step 5.

**Deliberate ordering.** Issue creation (3–4) precedes every deletion (7), and the anti-decision migration (6) precedes the deletion of the document that held it. Task 7 Step 1 refuses to delete unless 55 issues exist, so a silent failure in Task 4 cannot cascade into data loss.

**Known gap, stated rather than hidden.** Task 9's scratch deletion is the one step no `git revert` undoes. Its 171 review reports are per-commit findings already present in git history and claude-mem; this is accepted, not overlooked.
