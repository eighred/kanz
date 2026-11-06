# Kanz engineering boundaries

**Eighred** is the company. **Kanz** is its institutional investment operating
system: one financial model, controlled execution core and auditable state model
across secure interfaces. Risk, compliance, portfolio, margin, accounting and
audit belong to that model. The objective is to know the institutional state,
decide what is permitted, execute deterministically, record what happened and
explain why. An exchange connector or order router alone does not meet it.

## Instruction ownership

This file owns stable Kanz architecture, safety, environment and evidence rules.
The repository-owned [Kanz Forge plugin](.agents/plugins/kanz-forge/README.md)
owns engineering workflows, skill routing, specialist instructions and command
usage. Its source is `.agents/plugins/kanz-forge/`; the marketplace is
`.agents/plugins/marketplace.json`. The former private package was retired in
#1087. Do not create a second workflow manual or copy these invariants into each
skill. Forge cannot implicitly amend platform boundaries or grant authority.

For `/work`, `/ship` and related commands, use Forge's `commands` skill, which
selects the required investigation, engineering, verification and delivery
skills. Read the affected code, migrations and architecture guards before
changing a contract. Load supporting instructions for the task at hand rather
than every skill. Missing tools or an unavailable skill must be reported; their
presence in a manifest is not evidence of an operational capability.

## Planes and layout

The user-authorized architecture migration is tracked in #1413. Migrate this
repository in place: Rust for critical execution/financial/market-data components,
Go for orchestration and the BFF, the existing Vue client, and a JVM-free Redpanda
event transport target. Preserve existing features, identities, canonical schemas,
stored history and financial controls. Desktop prototypes are reference material,
not replacement contracts or deployment sources. Each component changes ownership
only after compatibility, recovery and rollback evidence; the current NATS spine,
single archiver and PostgreSQL writers remain authoritative until their explicit
cutover. Adding Rust compatibility tests does not activate a Rust trading path.

| Plane | Owns | Boundary |
|---|---|---|
| Execution | OMS, router, venue adapters, fills, exchange protocol and auth | Deterministic, typed, low-latency, fail-closed, independently testable. |
| Control | Risk, compliance, mandates, portfolio limits, leverage, margin, collateral, kill switches | Admission control: decides whether an action is permitted. |
| Intelligence | MCP, analytics, copilot, operational investigation | Reasons over state; never owns or becomes a dependency of execution. |

Identify the owning plane before editing. If AI, prompts, tool discovery or MCP
disappear, trading must continue. Moving execution into intelligence is an
architecture violation even when tests pass.

| Location | Responsibility |
|---|---|
| `kanz/` | Go module `github.com/eighred/kanz`; Go 1.26.1 language / 1.26.10 toolchain. Services in `services/`, binaries in `cmd/`. |
| `kanz-schemas/` | Protobuf + buf; generated Go SDK in `gen/go`. |
| `kanz-py/` | Python inference service. |
| `kanz-web/` | Browser client. |
| `kanz-rs/` | Rust feed runtimes, canonical compatibility primitives and diagnostic financial workers; ownership changes require component-specific #1413 acceptance. |
| `kanz-jl/` | Optional curve-calibration comparison worker; Go remains authoritative. |
| `kanz/infra/` | Deployment; Kubernetes and SPIFFE/SPIRE workload identity. |
| `tools/`, `.github/` | Host/operator scripts and CI. |

Keep sibling directories in place: Go replacements and build contexts depend on
them. Postgres uses pgx v5 and tenant RLS. NATS JetStream is the event spine;
Kafka (segmentio) is the derived archival/CDC path. Start setup at
`kanz/dev/README.md`, schema generation at `kanz-schemas/README.md`, and Python
setup at `kanz-py/README.md`. Keep operational runbooks beside their component;
do not move packages merely for tidiness.

## Execution and trust boundaries

```text
commands -> api-gateway -> NATS -> services -> Postgres
             authN/Z      FACTs    domain       RLS

strategy -> intent -> risk -> compliance -> mandate -> OMS
         -> execution router -> venue adapter -> exchange
```

State changes flow as FACTs on the bus, without polling. Go services are
`package main` under `services/<name>/cmd/<name>/`, with environment configuration
in `internal/config`. Shared code starts in a service's `internal/`; promote it
to `kanz/internal/` or `kanz/pkg/` only when a second consumer exists.

The gateway is the sole identity authority. It authenticates and injects
`X-Kanz-Principal-*`; upstream trust is sound only while NetworkPolicy makes the
gateway their only reachable caller. Do not trust those headers independently
of that network boundary.

Every order-path hop is typed and every step is mandatory, including for admins,
MCP and test helpers.
OMS to venue adapter uses typed gRPC/mTLS over `venue.v1`, gated by `buf breaking`
and capability declarations. Exchange REST/WS and signing live only in the
adapter. Changing either transport requires an architectural decision with its
own issue; it is not a refactor.

- **Three-state truth:** capability and financial state are `SUPPORTED`,
  `UNSUPPORTED` or `UNKNOWN`. An empty capability set asserts nothing. Critical
  unknowns fail closed: unknown margin must never become `0.0` and permit an
  order. Missing configuration and a successful check must remain distinct.
- **Credential isolation:** credentials terminate at the venue adapter. No
  credentials in source, git history, logs, MCP responses, agent context, audit
  facts, test artifacts or web responses.
- **MCP:** read-only, deny-by-default, narrowly scoped, server-side filtered and
  projected, using authorized domain APIs. It cannot place, cancel or amend
  orders, contact a venue directly or bypass OMS/risk/compliance. It is not the
  venue transport. Security and execution-path hardening outrank MCP work.
- **Attribution:** every order identifies strategy, portfolio, tenant, mandate,
  passed risk/compliance checks, venue capability/account, outbound request,
  venue response, fills and resulting state changes.
- **Canonical models:** domain concepts belong in the core; venue and endpoint
  shapes stay at the boundary. Money and quantities use `common.v1.Decimal`,
  never float.
- **Tenant isolation:** deny by default across portfolios, accounts, orders,
  positions, strategies, risk, mandates, credentials, audit and AI capabilities.
  One tenant must not even discover another tenant's state.
- **Commands and facts:** a command requests; a fact observes or commits.
  History must reconstruct knowledge, beliefs, controls and admission/rejection.
- **Recovery first:** never place real orders against a store that may not be
  backed up. Disaster-recovery coverage precedes live orders.

## Implementation and scale

Misconfiguration must fail loudly at the first event through denial, DLQ or
startup refusal, never a healthy-looking default. Keep one implementation per
concept so fixes reach every consumer. Comments explain operational consequences
and trade-offs; verify a comment's premise before relying on it.

Prefer existing typed contracts and authorization, canonical models, explicit
capabilities, server-side projection, deterministic execution, small migrations
and measured behavior. Preserve security boundaries over convenience. MCP cannot
justify rewriting execution; adapter deduplication must preserve isolation and
venue-specific semantics.

The TUI is retained until Web equivalents achieve operational parity and
production verification, followed by deliberate retirement. A route unused by
the web is not dead: `internal/tui/universe` owns calls to live gateway node
provisioning routes. Preserve every required human operation throughout migration.

Idempotent handlers are the correctness floor at every replica count. Per-process
state is a best-effort optimization that re-baselines on restart. Shared state is
an opt-in reduction of duplicate work, not a replacement for idempotency.

| Component | Default | Shared mode / seam |
|---|---|---|
| Bus dedup | Per-instance `DedupWindow` | `RedisDedup` via `bus.WithDeduper`; go-redis binding `redisadapter.New`. |
| PRED-09 model registry | Local `Registry` | `registry.CoordinatedRegistry` over `platform.model`. |
| Gap, staleness, watermark, drift detectors | Per-process, per-partition | No shared backend; `partition_key` already routes to one consumer in a group. |

Do not add shared state where partitioning gives single ownership. Shared
coordination backends fail open: a Redis error means no dedup, never a fabricated
hit that drops consumption. Idempotency makes that safe; this does not relax
critical financial admission controls.

`platform.model` is the infinite-retention append log of record (EVT-09).
Replicas replay from offset 0 into a local materialized registry, publish keyed
by `model_id` so `record_validation` precedes primary registration, and carry
producer `origin` so replicas skip their own events.

Kafka is derived from NATS through one archiver (DATA-M1). The old reconciler and
`internal/integrity` were removed (DATA-M4); preserve
`kanz/test/arch/drain_single_writer_gate_test.go` (#819) rather than rebuilding
two independent sources of truth.

## Verification rules

Use `.github/workflows/kanz-ci.yml` for actual CI jobs and `kanz/.golangci.yml`
for lint policy. These are independent gates:

```sh
cd kanz
go build ./...
go vet ./...
go test -p 1 ./internal/... -count=1
go test -p 1 ./services/... -count=1
go test -p 1 ./test/arch/ -count=1
golangci-lint run ./...
git ls-files -z '*.go' | xargs -0 gofmt -l

cd ../kanz-schemas
buf generate
bash bootstrap-go-sdk.sh
```

Run the relevant checks for the change, with these evidence constraints:

- `go test -p 1` is required: Postgres-gated tests share a database. Chunk tests
  rather than running one `./...`; the harness can reap a child at about ten
  minutes. Compare reported packages with `go list` to detect truncation even
  when the output contains no failures.
- Unset `TEST_POSTGRES_URL` silently skips database tests. Use a **NOSUPERUSER**
  role or RLS tests bypass isolation and pass falsely.
- `-race` requires cgo and does not run on the usual Windows host. Concurrency
  claims need CI evidence. `fakeBus` does not validate envelopes; it proves
  neither broker acceptance nor real messaging integration.
- Report tested commit, package coverage, actual dependencies and skipped tests.
  Local passes, CI, real integrations and production acceptance are distinct.
  A merge is not closure until acceptance is verified on the merged revision.
  Developer smoke setup (#1323) is not capital-path proof.
- Never use production DSNs or persist credentials in files, fixtures, commits
  or leftover environment files. External verification uses demo/testnet
  credentials for one run, rotated afterward.
- Respect sandbox and security blocks. Report the blocked action and what it
  would have proved; never reroute it or weaken safeguards to obtain a pass.

Read architecture guards in `kanz/test/arch/` before reporting a suspected gap.
They default-deny, with named exemptions tied to retirement issues and dead-entry
checks. Prefer an executable guard to another paragraph here. A passing guard
proves what it resolves, not its name; use an allowed mutation probe to verify
its reach. Do not remove an invariant until its replacement guard covers it.

For identity, RLS, message validation, signing or release changes, Forge must
route to `security-audit` and identify principal, resource and trust boundary.
Run the findings CLI on Linux: Windows lacks required no-follow/nonblocking
flags, which must remain enabled. The imported methodology is pinned and carries
its MIT notice; Node is a development-time validator dependency only.
Pricing, portfolio and backtest acceptance requires `financial-verification`;
production computation stays in its owning service or approved domain library;
diagnostic workers do not acquire financial authority.

## Windows environment

These requirements apply to CLI tools and editors. The examples below use Git
Bash; use equivalent native commands in PowerShell.

1. **Set `GOTMPDIR` before building.** Windows Application Control intermittently
   blocks test binaries under `%TEMP%`, causing incomplete runs. Set Go's own
   environment so all tools see it:

   ```sh
   mkdir -p "$HOME/.go-tmp"
   go env -w GOTMPDIR="$HOME/.go-tmp"  # undo: go env -u GOTMPDIR
   ```

   The Makefile overrides this with in-module `kanz/.gotmp` in CI. Use
   `git ls-files` for gofmt, never a tree walk that includes generated test mains.

2. **Generate the schema SDK.** `kanz/go.mod` replaces the SDK with
   `../kanz-schemas/gen/go`; ignored `gen/` is never committed (EVT-15a).
   Missing generated code causes unresolved imports in a fresh checkout. The
   host's downloaded `~/go/bin/buf.exe` is blocked; the established project-local
   source build is:

   ```sh
   GOBIN="$(pwd)/.gotmp/bin" go install github.com/bufbuild/buf/cmd/buf@v1.69.0
   ```

   If policy blocks that build too, report it rather than trying alternate
   routes. Run `buf generate` and `bash bootstrap-go-sdk.sh` in `kanz-schemas`
   after schema changes and on fresh checkouts.

3. **Use actual build tags.** There are no `binance` or `okx` tags; venue
   connectors use `github.com/coder/websocket` in the default build (#100).

   | Tag | Selects | Default (`!tag`) |
   |---|---|---|
   | `redis` | `pkg/redisadapter/goredis.go`, `risk-engine/dedup_redis.go`, `webhook-ingest/nonces_redis.go` | `dedup_default.go`, `nonces_default.go` |
   | `anthropic` | `copilot/model_anthropic.go` | `copilot/model_stub.go` |
   | `openrouter` | `copilot/model_openrouter.go` and tests | Provider absent |
   | `perf` | `internal/risk/compute/latency_budget_test.go` | No performance test |

   The default excludes go-redis and Anthropic SDK bindings, not venues. Pass
   tags to `gopls` too; for example untracked `.vscode/settings.json` can use
   `{ "go.buildTags": "redis anthropic" }`.

4. **Permit dependency resolution:** `go env -w GOFLAGS=-mod=mod`. This does not
   generate schemas; generation remains an explicit step.

5. **Diagnose line endings through Git.** `.gitattributes` pins Go and Protobuf
   to LF, but old worktrees can retain CRLF:

   ```sh
   git ls-files --eol '*.go'
   ```

   `i/lf` is committed LF; `w/crlf` is a local checkout condition. Restore only
   confirmed clean affected files after preserving local edits, then confirm
   `git status`. Do not commit line-ending churn. Do not use carriage-return
   grep counts: Git Bash escaping can become an empty pattern and falsely count
   every line as CRLF.

Git for Windows does not ship `make`. The Makefile remains the canonical task
list; use its underlying commands or install make through the normal setup.
The cgo/race limitation above remains a verification limitation.

## Durable context and work tracking

Use the repository and GitHub on `eighred/kanz`, not the last transcript, as
authority. At session start read these rules, working-tree status, current branch,
main's latest commit and relevant open PRs/issues. Respect the user's current
scope; when selecting work, prefer the highest-priority unfinished issue.

Stable rules live here, implementation beside its code and in git history,
decisions/blockers/deferred work in Issues, and implementation/verification
history in PRs. Forge workflows retrieve that context; a second memory database
or project-state document is not needed. Do not add plan files, project-state,
progress, status, roadmap, session-note or scratchpad documents anywhere in the
repository. Keep transient coordination in the chat; revalidate remembered facts
against the current revision.

For discovered defects, blockers or decisions outside the current PR, search
Issues first and reuse a matching issue before opening another. Record discovered
work before the session ends and link it from the PR, subject to explicit user
instructions about issue creation. Do not leave engineering decisions solely in
conversation or misrepresent unresolved work as complete.

Issues carry one kind (`open-work`, `needs-verification`, `blocked-external`,
`decision-needed`), one priority (`P0`/`P1`/`P2`) and an area label. Bodies state
Evidence (file/commit/result), Verified when (command and expected result),
Blocked by and Source. `needs-verification` means merged without its required
operational proof. Historical M0-M6 milestones (#55-#85) are not today's work
plan; M0, M1 and M6 are complete, and unmilestoned work is not unplanned.

Forge installation, verification and rollback belong in its README. Validate
the tracked plugin with `python .agents/plugins/kanz-forge/scripts/validate.py`;
`.github/workflows/kanz-forge.yml` checks its package and supporting references.
Update or roll back through a tracked checkout and supported plugin installation,
never by hand-editing the installed cache. Host discovery configuration is an
adapter, not a separate source of engineering instructions.
