# Local dev stack (DEVX-01a)

The development stack contains NATS, Kafka, Postgres, risk-engine and
api-gateway. It is a smoke environment, not proof of the complete capital path
or tenant isolation. Fresh-checkout tooling and Compose repairs are tracked in
[#1323](https://github.com/eighred/kanz/issues/1323); the current `make ci` is not
equivalent to repository CI. `make help` lists the available targets.

## Prerequisites

- Go (see `go.mod` for the version), Docker, and [buf](https://buf.build/docs/installation).
- The repo expects `kanz/` and `kanz-schemas/` as **sibling** directories — the
  Go module `replace`s the schema SDK to `../kanz-schemas/gen/go` (EVT-15a).
- **On Windows, read AGENTS.md's "Windows environment" section first.** A
  `GOTMPDIR` outside `%TEMP%`, `GOFLAGS`, build tags and the LF line-ending rule
  are required there, and skipping them makes `go test` fail at random while the
  suite silently under-runs.

## First run

```sh
cd kanz
make bootstrap     # install protoc-gen-go + protoc-gen-go-grpc
make generate      # buf generate → the schema SDKs (gen/go is NOT committed)
cd ../kanz-schemas
bash bootstrap-go-sdk.sh  # initialize gen/go/go.mod from reviewed dependencies
cd ../kanz
go build ./...
go vet ./...
golangci-lint run ./...
```

`make` requires a POSIX shell and is not bundled with Git for Windows. On
Windows, use the native Go commands and the environment setup in the root
`AGENTS.md`; run the schema bootstrap script through Git Bash. Install buf and
put both Go protobuf plugins on `PATH` before generation. Dependency tests need
disposable real services and their explicit test configuration; see the service
fixtures and `.github/workflows/kanz-ci.yml` for the current bootstrap sequence.
Never use a production DSN. PostgreSQL isolation tests require a role with both
`NOSUPERUSER` and `NOBYPASSRLS`; the Compose bootstrap role is not that role.

Run Go tests with `-p 1`, in bounded package groups (for example `./internal/...`,
then individual `./services/<name>/...`, then `./test/arch/`). Compare package
results with `go list` for each group and inspect skipped tests. A missing
`TEST_POSTGRES_URL` can produce green output without testing PostgreSQL. Run the
same relevant groups with `-race` on a cgo-capable host; Windows without cgo does
not establish race safety. Include `pkg`, `cmd`, tools and other test directories
when claiming a complete suite, not only the example groups above.

### Why `make generate` is step one

Generated code is never committed (EVT-15a). The kanz module `replace`s
`kanz-schemas-go` to `../kanz-schemas/gen/go`, so that tree must exist before
anything builds. `buf generate` needs `protoc-gen-go-grpc` on `PATH` — it emits
the inference + query gRPC stubs that `protoc-gen-go` alone omits — and `make
bootstrap` installs it. `GOFLAGS=-mod=mod` permits dependency resolution; it
does not regenerate schemas. After generation, `bootstrap-go-sdk.sh` initializes
the ignored Go SDK module using the consumer's reviewed dependency versions,
as CI does.

## Quick start

```sh
make up            # intended smoke loop; known setup defects are tracked in #1323
# ... develop ...
docker compose -f dev/docker-compose.yml down   # stop without deleting volumes
```

`make down` currently adds `-v` and deletes volumes. Reserve it for an intentional
reset of disposable data. `make up` currently suppresses seed failures, so its
exit status alone is not readiness evidence.

The repository also contains a Tilt configuration (live dashboard and logs):

```sh
tilt up
```

## What you get



| Service | Port | Notes |
|---|---|---|
| api-gateway | 8080 | the read surface (auth/signing/quotas OFF for dev) |
| risk-engine | 8082 (HTTP), 8081 (gRPC) | durable state in Postgres |
| NATS | 4222, 8222 | JetStream spine, `RISK` stream provisioned by `nats-init` |
| Kafka | 9092 | single-node KRaft (durable log for CDC/replay work) |
| Postgres | 5432 | `kanz/kanz`, db `kanz` |

```sh
curl localhost:8080/v1/portfolios/PF1/exposure   # the seeded portfolio
```

## Notes

- Build context is the repo root: the service Dockerfiles COPY the generated
  `kanz-schemas/gen/go` SDK, so run `make generate` before the first `make up`.
- Auth/signing/quotas are disabled — this is a dev loop, not a security posture.
- Seeding reuses `test/load/seed` (`SEED_NATS_URL` / `SEED_TENANT`).

## Creating a new service

```sh
make scaffold NAME=my-service     # EVT-16a layout, on-convention from line 1
go build ./services/my-service/...
```

The scaffold (DEVX-01b, `tools/scaffold`) emits `cmd/<name>/main.go` +
`internal/{config,server}` wired with observability + probes. Fill in the real
work; copy the bus-consumer pattern from `services/audit` or `services/lineage`.
The Backstage "New Kanz service" template (DEVX-01c) runs the same generator from
the portal.

## Everyday targets

| Target | What |
|---|---|
| `make generate` | regenerate the schema SDKs (after a `.proto` change) |
| `make build` | build binaries into `bin/` |
| `make test` / `make test-race` | current unchunked tests without / with race detection; use serialized groups above until #1323 is fixed |
| `make lint` | current `go vet` + gofmt check; does not run required golangci-lint |
| `make up` / `make down` | local dev stack |
| `make scaffold NAME=x` | new service skeleton |
| `make ci` | local convenience chain; actual CI is `.github/workflows/kanz-ci.yml` |

## Where things live

- Services: `services/<name>/` (entrypoint `cmd/<name>`, packages `internal/`).
- Shared libs: `pkg/` (cross-service) and `internal/` (cross-service, unexported).
- Schemas: the sibling `kanz-schemas/` module — proto + buf, and the five
  normative wire specifications in its `README.md`. SDKs generate into `gen/`.
- Infra: `infra/` (k8s, GitOps, security, observability, lakehouse, catalog,
  backstage). Portal: `infra/backstage`.
- Operational runbooks live beside the component they are about, in that
  component's `README.md` — `infra/dr/` for region failover and the drill,
  `infra/nats/` for broker incidents, `infra/deploy/` for replica loss,
  `infra/observability/slo/` for error-budget burn, `infra/tenancy/` for tenant
  onboarding, `services/autopilot/` and `services/audit/` for their own.
