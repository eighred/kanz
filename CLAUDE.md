# kanz

**Eighred** is the company. **Kanz** is Eighred's institutional investment
operating system: one financial model, one controlled execution core, one
auditable state model, several secure interfaces. It is not an exchange
connector, a trading bot, or a bag of venue APIs — execution is one component of
a system that also owns risk, compliance, portfolio, margin, accounting and
audit.

The architecture is institutional in the sense systems like BlackRock Aladdin
are: the book of record, the controls that govern it, and the execution that acts
on it are one coherent model rather than an order router with reporting bolted
beside it. That is the standard changes are judged against.

The objective is never "place an order on an exchange." It is: know the
institutional state, decide what is permitted, execute deterministically, record
what happened, and explain why.

## Planes

Know which plane you are in before changing anything.

| Plane | Owns | Rule |
|---|---|---|
| **Execution** | OMS, execution router, venue adapters, fills, exchange protocol and auth | Deterministic, typed, low-latency, fail-closed, independently testable. |
| **Control** | risk, compliance, mandates, portfolio limits, leverage, margin, collateral, kill switches | Admission control, not reporting. Decides *whether*. |
| **Intelligence** | MCP, analytics, copilot, operational investigation | Reasons over state. Never owns the execution path. |

Collapsing one plane into another is not a refactor. A change that makes
execution depend on the intelligence plane is wrong no matter how it tests.

## Stack

- **Go 1.26.1** — `kanz/`, module `github.com/eighred/kanz`. Services under
  `services/`, CLI binaries under `cmd/`.
- **Postgres** (pgx v5) — durable state, multi-tenant via RLS.
- **NATS JetStream** — the event spine. **Kafka** (segmentio) — the archival/CDC path.
- **Protobuf + buf** — `kanz-schemas/`, generated SDK at `kanz-schemas/gen/go`.
- **Python** — `kanz-py/`, the inference service.
- **Kubernetes + SPIFFE/SPIRE** — deployment and workload identity.

## Architecture

```
commands ──▶ api-gateway ──▶ NATS ──▶ services ──▶ Postgres
                (authN/Z)     (FACTs)   (per-domain)   (RLS)
```

- **Everything is event-driven.** State changes flow as FACTs on the bus; nothing polls.
- Services are `package main` under `services/<name>/cmd/<name>/`, configured from
  the environment in `internal/config`.
- Shared code starts in a service's `internal/`, and is promoted to `kanz/internal/`
  or `kanz/pkg/` only when a **second** consumer appears.
- The gateway is the sole identity authority: it authenticates and injects
  `X-Kanz-Principal-*`; upstreams trust those headers, which is sound **only**
  because a NetworkPolicy makes the gateway their only reachable caller.

The order path is fixed, and every hop is typed:

```
strategy ─▶ intent ─▶ risk ─▶ compliance ─▶ mandate ─▶ OMS
         ─▶ execution router ─▶ venue adapter ─▶ exchange
```

No step is skippable — not by MCP, not by an admin endpoint, not by a test helper
that "just needs a fill." If the AI layer disappears entirely, trading continues.
The execution core must not depend on an LLM, a prompt, dynamic tool discovery,
or MCP being up.

**The transports on that path are settled.** OMS → venue adapter is typed
gRPC/mTLS over `venue.v1`, and exchange REST/WS lives inside the venue adapter
and nowhere else. Both are load-bearing rather than incidental: the typed hop is
what `buf breaking` gates and what the capability declarations ride on, and
keeping the exchange protocol inside the adapter is what makes "credentials
terminate at the venue adapter" structural instead of a promise. Changing either
is an architectural decision with its own issue, not a refactor.

## Invariants

These are here because they change what you write. Each one has already cost
something.

- **`UNKNOWN` is a third value.** Capability and financial state are
  `SUPPORTED` / `UNSUPPORTED` / `UNKNOWN`. An empty capability set means *the
  venue did not assert support* — it does not mean unsupported. This is the same
  rule as "nothing configured" ≠ "checked, and fine," applied to money.
- **A critical unknown fails closed.** Unknown margin ratio ⇒ risk cannot
  establish safety ⇒ order refused. Never unknown ⇒ `0.0` ⇒ proceed as if known.
- **Credentials terminate at the venue adapter.** Signing logic and exchange auth
  live there and nowhere else. No credential in source, git history, logs, MCP
  responses, agent context, audit facts, test artifacts or web responses.
- **MCP is read-only, deny-by-default, narrowly scoped, server-side filtered and
  projected.** It may not place, cancel or amend orders, call a venue directly,
  or bypass OMS/risk/compliance. It reaches state through authorized domain APIs
  like any other caller. It is an agent-facing operational and read plane, and it
  is **not the venue transport** — a proposal to move OMS → adapter onto it is a
  proposal to replace a typed, gated, credential-isolating boundary with a
  discovery-oriented one, and the boundary it would provide already exists.
- **Security and execution-path hardening outrank MCP work**, always. MCP is an
  interface; a hole on the capital path is money. When both are open, the
  hardening lands first — and an MCP task never justifies deferring one.
- **Every order must be attributable.** For any order the system answers: which
  strategy, portfolio and tenant; which mandate permitted it; which risk and
  compliance checks passed; which venue capability and account were used; what
  was sent; what the venue returned; what filled; what state changed.
- **Canonical model in the core, venue shapes at the boundary.** The same concept
  must not be modelled differently by each integration, and endpoint-specific
  models are not a second domain model.
- **Tenant isolation is deny-by-default** and covers portfolios, accounts, orders,
  positions, strategies, risk state, mandates, credentials, audit and AI
  capabilities. One tenant's agent must not be able to *discover* another's state.
- **Commands and facts are distinct.** A command is requested; a fact is observed
  or committed. The history must reconstruct what the system knew, what it
  believed, which controls ran, and why the action was permitted or rejected.

## Coding standards

- **Fail loudly, never silently.** A misconfiguration must surface on the first
  event — a denial, a DLQ, a refusal to start. Never a default that looks healthy.
- **"Nothing configured" and "checked, and fine" must never look the same.**
- **One implementation per concept.** A copied helper is how a fix stops
  spreading: 17 services each had their own `secret()` and 15 were wrong while 2
  were right.
- **Comments carry the operational consequence**, not a restatement of the code.
  A comment justifying a trade-off is dated evidence — verify its premise before
  relying on it.
- Money and quantities are `common.v1.Decimal`; never float.

## Commands

```sh
cd kanz
go build ./...
go vet ./...
golangci-lint run ./...     # the FOURTH gate — build, vet and test all pass on code it rejects
git ls-files -z '*.go' | xargs -0 gofmt -l   # not `gofmt -l .` — GOTMPDIR is in-module (Windows environment #2), so a tree walk lists _testmain.go build artifacts as violations

# -p 1 is REQUIRED (see Constraints). Run in chunks, never as one ./... —
# the harness reaps a child at ~10 min, and a truncated run ends with 0 FAIL,
# which reads as green. Diff the packages reported against `go list` to prove
# the run finished.
go test -p 1 ./internal/... -count=1
go test -p 1 ./services/... -count=1
go test -p 1 ./test/arch/   -count=1

cd kanz-schemas && buf generate    # regenerate the Go/Python SDKs
```

## Constraints

- **`go test -p 1`** — the Postgres-gated tests share one database and race otherwise.
- **`TEST_POSTGRES_URL` unset ⇒ the Postgres-gated tests skip silently** — dozens
  of files, reported as `ok`. The role must be **NOSUPERUSER**, or RLS is
  bypassed and the isolation tests pass falsely.
- **`-race` needs cgo** — it does not run on the usual Windows box. Concurrency
  claims are unproven until CI runs them.
- **`fakeBus` does not validate envelopes**, so it accepts what a real broker
  rejects. A green suite using it is not a broker proof.
- Never commit secrets; never point a test at a production DSN. **Do not persist
  a credential anywhere** — not a file, not a fixture, not a commit, not an env
  file left behind after a probe. Verify external integrations with demo/testnet
  credentials only, passed in for one run and rotated after.
- **Do not work around a sandbox or security restriction.** A blocked action is a
  decision to respect and report, not an obstacle to route around — writing a
  credential to a file to dodge a classifier defeats the control and breaks the
  rule above at the same time. Say what was blocked and what it would have proved.

## Windows environment

Five things that make the project look broken — or make tests fail at random — if
you skip them. They are **environment**, not editor: they apply to the CLI, to
`go vet`, and to whatever editor points at the tree. Numbered, because the
`gofmt` note under Commands cites #2.

**1. Generate the schema SDK before anything else.** `kanz/go.mod` `replace`s the
schema SDK to `../kanz-schemas/gen/go` and `gen/` is gitignored (generated code is
never committed, EVT-15a). Skip it and you get thousands of unresolved imports —
the project is not broken, the SDK is missing. `~/go/bin/buf.exe` is permanently
blocked by Windows Application Control: a *downloaded* exe is blocked, one you
*build into a project-local directory* runs. So build it:

```sh
GOBIN="$(pwd)/.gotmp/bin" go install github.com/bufbuild/buf/cmd/buf@v1.45.0
```

**2. Move `GOTMPDIR` out of `%TEMP%` — do this first.** Windows Application
Control intermittently blocks freshly built test binaries in `%TEMP%`
(`Uygulama Denetimi ilkesi bu dosyayı engelledi`). `go test` then fails at random
and **the suite silently under-runs**, which is the dangerous part: a run that
never compiled half the packages still exits 0.

```sh
mkdir -p "$HOME/.go-tmp"
go env -w GOTMPDIR="$HOME/.go-tmp"     # undo with: go env -u GOTMPDIR
```

Set it in Go's own env so it reaches every tool, not just one shell. The Makefile
exports its own `GOTMPDIR` pointing at `kanz/.gotmp` for CI — that path is
*inside* the module and gitignored, which is why the fmt check asks git for the
file list rather than walking the tree.

**3. Build tags.** There are **no `binance`/`okx` tags and never were** — the
venue connectors are hand-rolled over `github.com/coder/websocket` and compile in
the default build (#100 retired that claim). Three tags exist:

| Tag | Selects | Default (`!tag`) |
|---|---|---|
| `redis` | `pkg/redisadapter/goredis.go`, `risk-engine/dedup_redis.go`, `webhook-ingest/nonces_redis.go` | `dedup_default.go`, `nonces_default.go` |
| `anthropic` | `copilot/model_anthropic.go` | `copilot/model_stub.go` |
| `perf` | `internal/risk/compute/latency_budget_test.go` | — |

The default build is vendor-free of go-redis and the Anthropic SDK — not of any
venue. Tags must reach `gopls` per-editor; in VS Code, `.vscode/settings.json`
(untracked): `{ "go.buildTags": "redis anthropic" }`. Without them those files
grey out with errors, and the `!tag` defaults beside them are what the untagged
build compiles.

**4. `go env -w GOFLAGS=-mod=mod`** so a stale generated SDK is regenerated
rather than failing the build.

**5. Line endings.** `.gitattributes` pins `*.go` and `*.proto` to `text eol=lf`
because gofmt emits LF; a CRLF working copy makes `gofmt -l` list files whose
formatting is perfect. The attribute only applies to files checked out *after* it
was added, so a clone predating it keeps CRLF forever.

**Diagnose with `git ls-files --eol`, never by grepping for carriage returns.**

```sh
git ls-files --eol '*.go' | awk '$2 == "w/crlf" {print $NF}'   # worktree is CRLF
git ls-files --eol '*.go' | awk '$1 != "i/lf"   {print $NF}'   # index is not LF
```

`i/lf` means the index — what git stores — is LF and there is nothing to commit;
`w/crlf` means your working copy is stale, fix it locally and do **not** commit.
Every `.go` file in this repo is already `i/lf`, so a `w/crlf` hit is always a
local artifact: `rm <files> && git checkout -- <files>`, then confirm `git status`
shows nothing modified.

The tempting check — `git show HEAD:file.go | grep -c $'\r'` — is wrong and has
already caused a false diagnosis. In Git Bash `$'\r'` does not reliably survive
into `grep`; degraded to an empty pattern it matches **every** line, and the count
comes back equal to the file's line count, which reads exactly like "every line
ends CRLF". That turns a local checkout artifact into an apparent committed-CRLF
problem whose "fix" is line-ending churn across files nobody touched.

**Not blockers, but know them:** `-race` requires cgo and does not run here (see
Constraints); `make` is not shipped with Git for Windows — the Makefile is the
canonical task list and works in CI/WSL/Linux, so on Windows either
`scoop install make` or run the underlying `go` commands.

## Coordination at N replicas

How stateful consumers behave when scaled out. Two guarantee classes, and the
choice is per component rather than platform-wide:

- **Best-effort (per-process, the default).** State lives in each replica's
  memory. Correct under scale-out *because the handlers are idempotent* — the
  per-process state is an optimization and an in-instance safety net, not the
  correctness boundary. Re-baselines on restart.
- **Strong (shared-state, opt-in).** State lives in a store every replica shares
  (Redis/Dragonfly, or the `platform.model` log), so replicas coordinate.

**The floor is always idempotent handlers.** Shared state reduces duplicate
*work* and false cross-replica divergence; it does not replace handler
idempotency, and where the shared operation cannot be atomic across pods the
handler is still the thing that makes a duplicate harmless.

| Component | Best-effort default | Shared-state mode | Seam |
|---|---|---|---|
| Bus dedup | `DedupWindow` (per-instance) | `RedisDedup` (cross-pod seen-set) | `bus.WithDeduper`; go-redis binding `redisadapter.New` |
| DATA-05 reconciler | in-memory `PendingStore` | `integrity.RedisPendingStore` (Lua-atomic) | `integrity.NewReconcilerWithStore` |
| PRED-09 model registry | process-local `Registry` | `CoordinatedRegistry` over `platform.model` | `registry.CoordinatedRegistry` |
| Gap / staleness / watermark / drift detectors | per-process, per-partition | — | n/a |

Three rules that are easy to get wrong, and each has a cost attached:

- **Do not add shared state where partitioning already gives single ownership.**
  The detectors key on a `partition_key` the bus already routes to one consumer
  in a group, so per-process state is correct without coordination.
- **`PendingStore.ClaimOrMatch` must be atomic per key.** It is a single
  check-set-or-delete; a non-atomic distributed implementation lets two replicas
  both record Pending and never match. Use the shared store whenever the NATS and
  Kafka feeds are consumed by *different* replicas — the common multi-replica
  case — or the two sightings never meet and both age out as **false**
  discrepancies.
- **Every shared backend fails OPEN, never closed.** A Redis error degrades dedup
  to no-dedup and `ClaimOrMatch` to Pending (never a fabricated match). An
  outage must not block consumption, and idempotent handlers are what makes that
  safe.

`platform.model` is the append log of record (infinite retention, EVT-09): a
starting replica replays it from offset 0 to rebuild the registry, so the topic
is the source of truth and the local `Registry` is a materialized cache. Publish
keyed by `model_id` so a model's `record_validation` precedes its primary
register in per-partition order, and carry the producing replica's `origin` so a
replica skips applying its own events.

## Changing the platform

Kanz evolves incrementally. Prefer, in this order:

existing typed contracts over parallel ones · existing authorization over a new
security system · canonical domain models over endpoint-specific ones · explicit
capabilities over implicit defaults · server-side projection over agent context ·
deterministic execution over agent-mediated execution · small migrations over
broad rewrites · existing precedent over speculative abstraction · measured
behavior over assumption · security boundaries over convenience.

Two consequences that are easy to get wrong:

- **MCP augments Kanz; it does not redefine it.** It is an interface
  architecture, and never a reason to rewrite execution. Deduplicating venue
  adapters is a separate piece of work that must not weaken isolation or flatten
  venue-specific semantics.
- **The TUI is retained for now, and is not deletable on sight.** The long-term
  direction is the Web app as the primary operator interface, and getting there
  is a deliberate TUI → Web *migration*, never dead-code cleanup: web equivalent
  → operational parity → production verification → retire → remove. **Unused by
  the web client is not evidence an operation is dead** — `internal/tui/universe`
  is today the only caller of two live gateway control routes, so deleting it on
  that reasoning would orphan node provisioning. Every required human operation
  must remain available before and after retirement.

## Where work is tracked

**GitHub Issues** on `eighred/kanz`. There is no task board, no plan files and no
architecture document in this repository; all three were tried and each became a
second answer competing with the code. Everything durable lives beside the code,
in git history, or in claude-mem.

**That extends to project state, and it is not negotiable.** Do not add
`project-state.md`, progress files, status files, roadmap files, session notes or
scratchpad documents — not under `docs/`, not anywhere. The persistent record is:
CLAUDE.md for stable rules, **Issues** for discovered work, blockers, deferred
work and decisions, **PRs** for implementation and verification history, and git
history for the code. A status file is a fifth answer that goes stale the day
after it is written, and this repository has already deleted 38 plan files and an
architecture document for exactly that reason.

**Discovered work goes to an Issue before the session ends.** If investigation
turns up a defect, a blocker, an architectural decision, or work outside the
current PR: search Issues first, reuse one if it fits, and open one if it does
not. No engineering decision or discovered defect should exist only in a
conversation. Reference the issue from the PR so discovery → implementation →
verification stays connected.

**Starting a session, read the repository rather than the last transcript**:
CLAUDE.md, `git status`, the current branch, open PRs, open Issues, and main's
latest commit. Then pick the highest-priority unfinished issue. A previous
conversation is not state.

The `M0`…`M6` milestones covered issues #55–#85 and were then abandoned: M0, M1
and M6 are **closed and complete**, and most open issues carry no milestone at
all. Read them as history. Do not force-fit new work into them, and do not treat
an unmilestoned issue as unplanned.

Every issue carries one **kind** label (`open-work`, `needs-verification`,
`blocked-external`, `decision-needed`), one **priority** (`P0`/`P1`/`P2`), and an
**area**. `needs-verification` means *merged, and nothing has run it* — a
distinct state from done, because a merged fix and a green suite are not
evidence.

An issue body states four things: **Evidence** (`file:line`, a commit, or a
quoted result), **Verified when** (a runnable command and its expected result),
**Blocked by**, and **Source**.

### Sequencing that outlives any one issue

- **Never place real orders against a store that may not be backed up.** The one
  ordering error with an unrecoverable failure mode. DR coverage lands first.

## Skills

Long procedures live in skills, not here.

| Skill | For |
|---|---|
| `kanz-forge:engineering-standard` | what "good" means; whether work is worth doing |
| `kanz-forge:kanz-verify` | what counts as proof, and the traps that have produced false green |
| `kanz-forge:stack-routing` | which layer owns a job; which artifacts must not be created |

**An invariant worth keeping is a guard, not a paragraph.** The arch tests in
`kanz/test/arch/` enforce these rules — default-deny, with named exemptions that
carry the issue retiring them and a dead-entry check so an exemption cannot
outlive its repair. Add one there rather than a rule here. The Invariants section
above is the exception, not the precedent: each line is there because no guard
covers it yet, and each should leave when one does.

Read them before filing a bug: an absence that looks like an oversight is often
a guarded decision, and the exemption states why. A guard that passes proves
what it *resolved*, not what its name claims — mutate the artifact to find out
which.
