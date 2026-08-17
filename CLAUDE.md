# kanz

Eighred is an institutional-grade trading and risk management platform that unifies
order management, risk analytics, accounting, and compliance within a scalable
multi-tenant architecture designed for fund managers and investment firms.

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

## Commands

```sh
cd kanz
go build ./...
go vet ./...
golangci-lint run ./...     # the FOURTH gate — build, vet and test all pass on code it rejects
git ls-files -z '*.go' | xargs -0 gofmt -l   # not `gofmt -l .` — GOTMPDIR is in-module (windows-go-setup.md #2), so a tree walk lists _testmain.go build artifacts as violations

# -p 1 is REQUIRED (see Constraints). Run in chunks, never as one ./... —
# the harness reaps a child at ~10 min, and a truncated run ends with 0 FAIL,
# which reads as green. Diff the packages reported against `go list` to prove
# the run finished.
go test -p 1 ./internal/... -count=1
go test -p 1 ./services/... -count=1
go test -p 1 ./test/arch/   -count=1

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
- **`TEST_POSTGRES_URL` unset ⇒ the Postgres-gated tests skip silently** — dozens
  of files, reported as `ok`. The role must be **NOSUPERUSER**, or RLS is
  bypassed and the isolation tests pass falsely.
- **`-race` needs cgo** — it does not run on the usual Windows box. Concurrency
  claims are unproven until CI runs them.
- **`fakeBus` does not validate envelopes**, so it accepts what a real broker
  rejects. A green suite using it is not a broker proof.
- Never commit secrets; never point a test at a production DSN.

## Where work is tracked

**GitHub Issues** on `eighred/kanz`. There is no task board, no plan files and no
architecture document in this repository; all three were tried and each became a
second answer competing with the code. Everything durable lives beside the code,
in git history, or in claude-mem.

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
outlive its repair. Add one there rather than a rule here.

Read them before filing a bug: an absence that looks like an oversight is often
a guarded decision, and the exemption states why. A guard that passes proves
what it *resolved*, not what its name claims — mutate the artifact to find out
which.
