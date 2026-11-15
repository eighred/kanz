# Kanz Go module

This is the established `github.com/eighred/kanz` Go module inside Eighred's
multi-runtime Kanz repository. It contains domain services, the existing
financial/admission owners, enterprise APIs, the web BFF and operator tools.
The [root guide](../README.md) explains the product and sibling runtimes.

## Placement

| Path | Responsibility |
|---|---|
| `services/` | Service composition roots, private implementation, owned migrations and runbooks. |
| `cmd/` | Operator/administrative entrypoints; released names remain stable. |
| `internal/` | Module-private domain and platform owners shared by actual consumers. |
| `pkg/` | Deliberately reusable APIs; avoid creating a generic utilities layer. |
| `test/` | Architecture, cross-component, load and qualification checks. |
| `tools/` | Go-module-local generators and conformance/oracle tools. |
| `ts/` | TypeScript bus client and wire-parity tests. |
| `dev/`, `infra/` | Local smoke setup and operational/deployment components. |

Services use `services/<source_name>/cmd/<source_name>/` and private
`internal/config/`. Source directories use snake_case; binary/image/service
identities such as `risk-engine` remain separate contracts. The Go module name
and the generated schema SDK module identity are unchanged.

## Build and verify

Follow [developer setup](dev/README.md) and [AGENTS.md](../AGENTS.md), particularly
Windows `GOTMPDIR`, generation and serial-test requirements. Go language and
toolchain versions are pinned in `go.mod`; lint policy is `.golangci.yml`.
The sibling `schemas/gen/go` output must be generated and initialized first.

From this directory on a configured POSIX host:

```sh
make bootstrap
make generate
go build ./...
go vet ./...
go test -p 1 ./test/arch/ -count=1
make fmt
```

Run the affected domain/package suites as well. Database/broker/browser/native
tests need their real disposable dependencies and explicit settings; report skips.
`make build` and `make release` emit executable names expected by operators and
the TUI. `make help` lists tasks; Git for Windows does not supply make.

The [Eighred organization standard](../.agents/plugins/kanz-forge/skills/repository-organization/references/eighred-standard.md)
governs placement and moves. Deployment acceptance and financial authority
cannot be inferred from a directory, local compile or generated SDK.
