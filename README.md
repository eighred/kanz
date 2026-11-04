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

| Path | Contents |
|---|---|
| [`kanz/`](kanz/) | Go services, shared libraries, command-line tools, migrations, and architecture tests. |
| [`kanz-schemas/`](kanz-schemas/README.md) | Protobuf contracts, event specifications, and generated SDK tooling. |
| [`kanz-web/`](kanz-web/) | Vue operator interface. |
| [`kanz-py/`](kanz-py/README.md) | Python event-bus client and inference service. |
| [`kanz/infra/`](kanz/infra/README.md) | Deployment, infrastructure, workload identity, and operational configuration. |
| [`tools/`](tools/) | Host and operator tooling. |
| [`.github/workflows/`](.github/workflows/) | CI, security, schema, and release workflows. |
| [`.agents/plugins/kanz-forge/`](.agents/plugins/kanz-forge/README.md) | The repository-owned Codex engineering assistant. |

Keep the Go module and schema directories as siblings: local SDK resolution
and build contexts depend on that layout. Component runbooks stay beside their
code.

## Development

Start with [the developer guide](kanz/dev/README.md) and [AGENTS.md](AGENTS.md).
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
It selects specialist workflows from the task's actual changes and coordinates
independent native workers while CI runs. Start with `Kanz /work` or
`Kanz /ship #<PR>` in Codex Chat.

See the [Forge guide](.agents/plugins/kanz-forge/README.md) for installation,
parallel work, capabilities, and verification limits. Forge is development
tooling; it does not grant trading, infrastructure, or deployment authority.

## Security and contribution boundaries

Read [SECURITY.md](SECURITY.md) for reporting procedures and
[AGENTS.md](AGENTS.md) before changing a contract or trust boundary. Keep
credentials out of source, logs, test artifacts, and agent context. Use
testnet/demo inputs for external verification. The repository's terms are in
[LICENSE.md](LICENSE.md).
