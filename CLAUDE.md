# kanz

Eighred is an institutional-grade trading and risk management platform that unifies
order management, risk analytics, accounting, and compliance within a scalable
multi-tenant architecture designed for fund managers and investment firms.

## Stack

- **Go 1.26.1** — `kanz/`, module `github.com/eighred/kanz`. 26 services, 12 CLI binaries.
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

## Commands

```sh
cd kanz
go build ./...
go test -p 1 ./...          # -p 1 is REQUIRED — see Constraints
go vet ./...
git ls-files -z '*.go' | xargs -0 gofmt -l   # not `gofmt -l .` — GOTMPDIR is in-module (windows-go-setup.md #2), so a tree walk lists _testmain.go build artifacts as violations

cd kanz-schemas && buf generate    # regenerate the Go/Python SDKs
```

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

## Constraints

- **`go test -p 1`** — the Postgres-gated tests share one database and race otherwise.
- **`TEST_POSTGRES_URL` unset ⇒ 14 test files skip silently.** The role must be
  **NOSUPERUSER**, or RLS is bypassed and the isolation tests pass falsely.
- **`-race` needs cgo** — it does not run on the usual Windows box. Concurrency
  claims are unproven until CI runs them.
- **`fakeBus` does not validate envelopes**, so it accepts what a real broker
  rejects. A green suite using it is not a broker proof.
- Never commit secrets; never point a test at a production DSN.

## Where work is tracked

**GitHub Issues** on `eighred/kanz` — milestones `M0`…`M6`. There is no task
board, no plan files and no architecture document in this repository; all three
were tried and each became a second answer competing with the code. Everything
durable lives beside the code, in git history, or in claude-mem.

Every issue carries one **kind** label (`open-work`, `needs-verification`,
`blocked-external`, `decision-needed`), one **priority** (`P0`/`P1`/`P2`), and an
**area**. `needs-verification` means *merged, and nothing has run it* — a
distinct state from done, because a merged fix and a green suite are not
evidence.

An issue body states four things: **Evidence** (`file:line`, a commit, or a
quoted result), **Verified when** (a runnable command and its expected result),
**Blocked by**, and **Source**.

### Sequencing that outlives any one issue

- **M0 first, and not partially.** Everything after it is verified by a pipeline
  that cannot presently complete a merge unaided.
- **M3 must not precede the DR-coverage issue in M0.** Placing real orders
  against a store that may not be backed up is the one ordering error with an
  unrecoverable failure mode.

## Skills

Long procedures live in skills, not here.

| Skill | For |
|---|---|
| `kanz-forge:engineering-standard` | what "good" means; whether work is worth doing |
| `kanz-forge:kanz-verify` | what counts as proof, and the traps that have produced false green |
| `kanz-forge:stack-routing` | which layer owns a job; which artifacts must not be created |

**An invariant worth keeping is a guard, not a paragraph.** 42 arch tests in
`kanz/test/arch/` enforce these rules — default-deny, with named exemptions that
carry the issue retiring them and a dead-entry check so an exemption cannot
outlive its repair. Add one there rather than a rule here.
