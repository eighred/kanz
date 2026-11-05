# Local dev stack (DEVX-01a)

The development stack contains NATS, Redpanda (Kafka API), Postgres, risk-engine and
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
make generate      # buf generate + initialize the ignored Go SDK module
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

Architecture tests invoke `bash` by name. In PowerShell, check
`Get-Command bash -All` before running them. If Windows resolves the unconfigured
WSL launcher, select the installed Git Bash for the current process (adjust the
installation path if necessary):

```powershell
$env:PATH = 'C:\Program Files\Git\bin;C:\Program Files\Git\usr\bin;' + $env:PATH
Get-Command bash
```

An `execvpe(/bin/bash) failed` result is a shell setup failure; it does not
exercise the script's refusal or retry behavior.

Run Go tests with `-p 1`, in bounded package groups (for example `./internal/...`,
then individual `./services/<name>/...`, then `./test/arch/`). Compare package
results with `go list` for each group and inspect skipped tests. A missing
`TEST_POSTGRES_URL` can produce green output without testing PostgreSQL. Run the
same relevant groups with `-race` on a cgo-capable host; Windows without cgo does
not establish race safety. Include `pkg`, `cmd`, tools and other test directories
when claiming a complete suite, not only the example groups above.

### Why generation precedes builds

Generated code is never committed (EVT-15a). The kanz module `replace`s
`kanz-schemas-go` to `../kanz-schemas/gen/go`, so that tree must exist before
anything builds. `buf generate` needs `protoc-gen-go-grpc` on `PATH` — it emits
the inference + query gRPC stubs that `protoc-gen-go` alone omits — and `make
bootstrap` installs it. `GOFLAGS=-mod=mod` permits dependency resolution; it
does not regenerate schemas. `make generate` runs `bootstrap-go-sdk.sh` after
generation to initialize the ignored Go SDK module using the consumer's
reviewed dependency versions, as CI does.

## Quick start

```sh
make up            # generates the SDK, then starts the smoke stack and seeds it
# ... develop ...
make down          # stop without deleting volumes
```

`make up` propagates seeding errors. Its exit status is still not proof of the
capital path; this stack lacks OMS, compliance, mandates and venue adapters.

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
| Redpanda (`kafka` alias) | 127.0.0.1:9092 | single-node Kafka-compatible broker; container clients use kafka:9092 |
| Postgres | 5432 | `kanz/kanz`, db `kanz` |

```sh
curl localhost:8080/v1/portfolios/PF1/exposure   # the seeded portfolio
```

## Notes

### Local broker migration

The existing `kafka` service now runs the version-pinned Redpanda `v26.2.6`
image in place of Bitnami Kafka. There is one broker, not a parallel replacement
platform. Separate internal and external advertised listeners allow both Compose
clients (`kafka:9092`) and host tests (`127.0.0.1:9092`) to follow broker metadata.
Only the external Kafka listener is published, on host loopback. The resource
flags and listener layout follow Redpanda's
[single-broker example](https://docs.redpanda.com/labs/docker-compose/single-broker/).
We explicitly disable topic auto-creation and write caching, use replication
factor one and pass `--unsafe-bypass-fsync=false`. The image can inherit
[`dev-container` defaults](https://docs.redpanda.com/streaming/current/reference/rpk/rpk-redpanda/rpk-redpanda-start/)
even without an explicit mode argument, so omitting the mode is insufficient.
Each `--set` and its value are separate command tokens, as required by rpk's
[`parseConfigKvs` implementation](https://github.com/redpanda-data/redpanda/blob/dev/src/go/rpk/pkg/cli/redpanda/start.go).
This remains a local
plaintext smoke environment, not a production durability or authorization proof.

The named `redpanda-data` volume is new storage; existing Kafka storage is never
mounted or converted. `make down` retains it. To roll back the local broker,
stop the smoke stack and restore the previous Compose broker definition before
starting it again; retain both stores separately. This does not transfer
Redpanda history or consumer offsets back into Kafka. Do not delete volumes as
part of rollback.

NATS JetStream remains the live event spine. The existing single archiver remains
the NATS-to-Kafka-API writer, preserving original envelope bytes and acknowledging
only after successful archive/DLQ publication. Its single-writer/drain gate and
all financial authority stay unchanged. This smoke stack does not run archiver
or lake-sink, so starting its broker does not prove archival/replay compatibility.
Production Kafka manifests, security configuration, stored history and migration
are outside this local change.

Topics must be explicitly provisioned by the relevant integration fixture; the
local broker does not automatically provision the production topic catalog.
For dependency tests use `TEST_KAFKA_BROKERS=127.0.0.1:9092` and the real NATS
fixture configuration. Required compatibility evidence includes bus produce/read,
lag and dead-DLQ behavior, archiver ordering/outage/drain/tenant cases, replay
ranges and lake-sink durability. A YAML/config validation pass is not runtime
evidence; record actual suite results and dependency skips separately.

- Build context is the repo root: the service Dockerfiles COPY the generated
  `kanz-schemas/gen/go` SDK. `make up`, build, vet and test run generation first.
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
| `make test` / `make test-race` | serialized bounded package groups without / with race detection |
| `make lint` | go vet, tracked gofmt check and golangci-lint |
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
