# Kanz

Kanz is Eighred's institutional investment operating system. It brings the book
of record, portfolio and risk controls, execution, accounting, and audit into
one system: establish the institutional state, decide what is permitted,
execute deterministically, and record what happened and why.

## How it fits together

Kanz separates three responsibilities:

| Plane | Responsibility |
|---|---|
| Execution | Order management, routing, venue adapters, fills, and exchange protocols. |
| Control | Risk, compliance, mandates, portfolio limits, margin, collateral, and kill switches. |
| Intelligence | Analytics, inference, copilot, and authorized operational investigation. |

The order path is typed and controlled at every step:

```text
strategy → intent → risk → compliance → mandate → OMS → router → venue adapter
```

The gateway authenticates callers. Domain services commit durable state in
Postgres and publish facts through NATS JetStream; Kafka serves the archival
and CDC path. Protobuf defines the shared wire contracts. Venue credentials
terminate at the adapter. Intelligence interfaces can inspect authorized state;
they cannot bypass admission controls or become a dependency of execution.

These are architectural boundaries, not a claim that every deployment or
external integration has been verified. Acceptance evidence and remaining work
are tracked in [GitHub Issues](https://github.com/eighred/kanz/issues) and PRs.

## Repository layout

| If you are working on | Start here |
|---|---|
| Live risk decisions and financial state | [Go risk engine](kanz/services/risk_engine/), [risk calculations](kanz/internal/risk/), and [OMS](kanz/services/oms/) in the `kanz/` Go module. |
| Market-data runtimes and Rust migration work | [Rust workspace](rust/README.md), including the feed runtimes and the diagnostic [bond-risk worker](rust/services/bond_risk_shadow/README.md). |
| Independent curve calibration | [Julia worker](julia/README.md). It compares against the installed Go curve. |
| Shared wire contracts and SDK generation | [Protobuf schemas](schemas/README.md) in `schemas/`. |
| Operator interface | [Vue client](web/). |
| Authorized inference | [Python inference service](python/README.md). |
| Deployment, operations, and CI | [Infrastructure](kanz/infra/README.md), [operator tools](tools/), and [workflows](.github/workflows/). |

See [Directory responsibilities](AGENTS.md#directory-responsibilities) for naming and moving
components as the repository evolves.

The migration in [#1413](https://github.com/eighred/kanz/issues/1413) is incremental
and incomplete. Rust feed runtimes support explicit depth ownership handoff;
Go retains trades and authoritative financial state. Julia calibration and Rust
bond-risk/fill-fold comparisons are disabled by default and cannot replace Go
results, persist financial state, or authorize execution. Source and CI completion
do not establish production rollout.

## Development

Start with [CONTRIBUTING.md](CONTRIBUTING.md), [the developer setup](kanz/dev/README.md)
and [AGENTS.md](AGENTS.md).
The latter records platform invariants, verification constraints, and Windows
environment requirements. The current Go version is specified in
[`kanz/go.mod`](kanz/go.mod).

Generate the schema SDK before building; generated code is not committed.
On a configured POSIX development host:

```sh
cd kanz
make bootstrap
make generate
go build ./...
go vet ./...
go test -p 1 ./internal/... -count=1
golangci-lint run ./...
```

This test command covers one package group. Run the affected service and
architecture groups as well; use the actual [CI jobs](.github/workflows/kanz-ci.yml)
for the complete gate set. Dependency tests require disposable real services
and explicit configuration. A skipped test does not verify its dependency.
The local Compose stack is a smoke environment, not complete capital-path proof.

## Kanz Forge

Forge helps Codex investigate, implement, review, verify, and deliver Kanz work.
Describe the outcome in Codex Chat. Forge's routing guide helps Codex choose
specialist workflows and run the checks the change needs. Use `Kanz /work` when
you want Codex to select and deliver unfinished work.

See the [Forge guide](.agents/plugins/kanz-forge/README.md) for installation,
repository discovery, organization standards, parallel work and verification limits. Forge is development
tooling; it does not grant trading, infrastructure, or deployment authority.

## Security and contribution boundaries

Read [SECURITY.md](SECURITY.md) for reporting procedures and
[AGENTS.md](AGENTS.md) before changing a contract or trust boundary. Keep
credentials out of source, logs, test artifacts, and agent context. Use
testnet/demo inputs for external verification. The repository's terms are in
[LICENSE.md](LICENSE.md).
