# Kanz

Kanz is Eighred's institutional investment operating system: one controlled
model for portfolio state, risk, compliance, execution, accounting and audit.
Changes are judged by production readiness, not feature count.

## System boundaries

| Plane | Owns | Boundary |
|---|---|---|
| Execution | OMS, router, venue adapters, fills | Typed, deterministic, fail-closed. |
| Control | Risk, compliance, mandates, margin, collateral | Admission control: decides whether action is permitted. |
| Intelligence | MCP, analytics, copilot | Read/investigation plane; never owns execution. |

The capital path is fixed and every hop is typed:

```text
strategy -> intent -> risk -> compliance -> mandate -> OMS
         -> execution router -> venue adapter -> exchange
```

OMS to venue adapter is `venue.v1` gRPC/mTLS. Exchange REST/WebSocket and
credentials terminate inside the adapter. Changing either boundary requires an
explicit architectural decision.

## Non-negotiable invariants

- Financial and capability state is `SUPPORTED`, `UNSUPPORTED` or `UNKNOWN`.
  Empty is unknown. Critical unknowns fail closed.
- Every order is attributable to tenant, portfolio, strategy, mandate, risk and
  compliance decisions, venue capability/account, request, response, fills and
  resulting state.
- Commands request change; FACTs record observed or committed change. Do not
  blur them.
- Tenant isolation is deny-by-default across state, credentials, audit and AI
  interfaces.
- Money and quantities use `common.v1.Decimal`, never floating point.
- Canonical domain models live in the core; venue-specific shapes stay at the
  boundary.
- MCP is read-only, server-filtered and deny-by-default. Trading must continue
  if MCP or every LLM disappears.
- Misconfiguration fails at startup or on the first affected event. Nothing
  configured must never look like checked-and-safe.
- Never put credentials in source, git history, logs, prompts, responses,
  fixtures or persistent local files. Use demo/testnet credentials for external
  verification and rotate them afterward.
- Never place a real order against infrastructure without current DR proof.

## Repository shape

- Go 1.26.1 module: `kanz/` (`github.com/eighred/kanz`)
- Protobuf/buf schemas: `kanz-schemas/`; generated SDKs are untracked
- Python inference service: `kanz-py/`
- Durable state: Postgres/pgx with RLS
- Event spine: NATS JetStream; Kafka is the archival/CDC path
- Deployment identity: Kubernetes and SPIFFE/SPIRE

Services are `package main` under `kanz/services/<name>/cmd/<name>/`. Shared
code begins in a service's `internal/` and is promoted only when a second real
consumer exists.

## Working rules

- Read code, tests, current git state and GitHub Issues before relying on prose
  or prior conversation.
- Preserve unrelated working-tree changes. Never use destructive git restore
  commands unless explicitly requested.
- Prefer an existing typed contract, authorization path, domain model and
  operational pattern over a parallel abstraction.
- Keep plans, decisions, blockers and discovered work in GitHub Issues. Keep
  implementation and verification history in PRs/git. Do not add roadmap,
  project-state, session-note, architecture or plan files.
- An invariant worth retaining belongs in `kanz/test/arch/` as a meaningful,
  mutation-proven guard. Do not add source-text tests that only mirror prose.
- The TUI remains until the Web interface has verified operational parity.
- Report verification limits explicitly. A skipped integration test, a test
  process that never executed, or a suite that under-ran is not green evidence.

Every issue uses one kind label (`open-work`, `needs-verification`,
`blocked-external`, `decision-needed`), one priority (`P0`, `P1`, `P2`) and one
area. Its body records Evidence, Verified when, Blocked by and Source. Search
before filing; discovered work must be recorded before the session ends.

## Verification entry points

Generate schemas before Go verification on a fresh checkout. From `kanz/`:

```sh
go build ./...
go vet ./...
go test -p 1 ./internal/... -count=1
go test -p 1 ./services/... -count=1
go test -p 1 ./test/arch/ -count=1
golangci-lint run ./...
git ls-files -z '*.go' | xargs -0 gofmt -l
```

`-p 1` is mandatory because Postgres-gated packages share a database. An unset
`TEST_POSTGRES_URL` silently skips integration coverage; the role must be
`NOSUPERUSER` for RLS tests. Race evidence requires a supported CGO toolchain.

Use the repository-local Codex skills for conditional detail:

- `kanz-engineering` for architecture, implementation and review decisions.
- `kanz-triage` when selecting what work should happen next.
- `kanz-verify` before claiming a fix, suite, issue or deletion is complete.
