# Local dev stack (DEVX-01a)

The development stack contains NATS, Redpanda (Kafka API), Postgres, risk-engine and
api-gateway. It is a smoke environment, not proof of the complete capital path
or tenant isolation. Fresh-checkout tooling and Compose repairs are tracked in
[#1323](https://github.com/eighred/kanz/issues/1323); the current `make ci` is not
equivalent to repository CI. `make help` lists the available targets.

## Prerequisites

- Go (see `go.mod` for the language/toolchain pin), Docker with a working Linux
  engine and Compose v2, Python 3, GNU make, a POSIX shell, and
  [buf 1.69.0](https://buf.build/docs/installation). `make generate` refuses a
  different buf version. `make bootstrap` installs the pinned Go protobuf plugins;
  put `$(go env GOPATH)/bin` on PATH. For lint, use the golangci-lint version in
  `.github/workflows/kanz-ci.yml`.
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
`NOSUPERUSER` and `NOBYPASSRLS`. Compose creates `kanzapp` with both restrictions;
the bootstrap role `kanz` is reserved for initialization. The live smoke service
uses `kanz_risk_dev`; destructive test fixtures use the separate `kanzapp` database.

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

`make test` and `make test-race` now inventory `go list ./...`, run batches of at
most 20 packages serially with fresh results, and reject incomplete JSON event
streams, failed/cached results and missing packages. Every skipped test is
reported; a complete package inventory still does not prove skipped dependencies.
For a targeted check use `python3 dev/check.py test ./internal/risk/state/persist`.
On Windows use `python`, or set `PYTHON=python` when running make. `make fmt`
(`fmt-check` alias) includes tracked and untracked Go files, handles spaces and
deleted files, and excludes ignored generated/build output.

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
make up            # generates/builds, initializes, waits, seeds and checks the read path
# ... develop ...
make down          # stop without deleting volumes
```

`make up` propagates build, canonical NATS bootstrap, migration, readiness and
seed/read failures. A successful check requires an unauthenticated 401 and an
authenticated `dev`/`PF1` exposure containing the four seeded instruments.
All published ports bind to `127.0.0.1`. This stack lacks OMS, compliance,
mandates and venue adapters; it does not prove the capital path.

NATS, PostgreSQL and Redpanda use project-owned named volumes. Ordinary shutdown
preserves them. For an intentionally destructive fresh start, use
`make reset CONFIRM=kanz-dev`; this deletes only the selected Compose project's
volumes. Use `COMPOSE_PROJECT_NAME` to isolate parallel checkouts (host ports
must also be free). Existing volumes are never silently repaired or deleted:
PostgreSQL init scripts run only on an empty data directory. If using an older
stack, preserve its data and choose a fresh project for this new schema/role
bootstrap. Do not point test fixtures at `kanz_risk_dev`.

The repository also contains a Tilt configuration (live dashboard and logs):

```sh
tilt up
```

## What you get



| Service | Port | Notes |
|---|---|---|
| api-gateway | 8080 | authenticated read surface, explicit public dev HS256 identity |
| risk-engine | 8082 (HTTP), 8081 (gRPC) | durable state in Postgres |
| NATS | 4222, 8222 | JetStream spine, canonical stream bootstrap via `nats-init` |
| Redpanda (`kafka` alias) | 127.0.0.1:9092 | single-node Kafka-compatible broker; container clients use kafka:9092 |
| Postgres | 5432 | restricted `kanzapp/kanz`; separate service and test databases |

```sh
python3 dev/smoke.py  # verifies unauthenticated denial and authenticated seeded read
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
- Authentication uses the explicit public dev HS256 opt-in; mTLS and rate limits
  are absent. This is a loopback smoke stack, not a production security posture.
- Seeding reuses `test/load/seed` with tenant `dev`, matching the risk engine.
- `risk-migrate` runs the canonical migration image before risk-engine startup.
  The restricted role owns the smoke schema; FORCE RLS still applies. This is
  not production separation between a DDL owner and the runtime role.

### Disposable dependency verification

After startup, verify the fixture role and run the real persistence tests:

```sh
docker compose -f dev/docker-compose.yml exec -T postgres psql -U kanz -d kanz \
  -Atc "SELECT rolname,rolsuper,rolbypassrls FROM pg_roles WHERE rolname='kanzapp'"
# Required: kanzapp|f|f
export TEST_POSTGRES_URL='postgres://kanzapp:kanz@127.0.0.1:5432/kanzapp?sslmode=disable'
python3 dev/check.py test ./internal/risk/state/persist
TEST_KAFKA_BROKERS=127.0.0.1:9092 go test -p 1 -count=1 -run '^TestKafkaPublishSubscribe$' -v ./pkg/bus
```

These are public disposable credentials. Do not substitute a production DSN.
The RLS test must run, not skip. Host Kafka publish/read exercises the external
advertised listener; `rpk cluster info -X brokers=kafka:9092` inside the Kafka
container checks the internal advertised address. NATS initialization and the
host seed-to-container-risk-to-gateway check exercise both sides of that topology.
The wider integration suite still needs its declared dependencies, including
the routed broker config in `test/backing/nats-dev.conf`, separate live OMS DB,
mTLS broker and Python inference worker. The smoke stack does not supply those.

Rust, Julia and browser qualification remain separate from this Go smoke target:
use `kanz-rs/README.md`, `kanz-jl/README.md`, `kanz-web/README.md` and their owning
workflows for the pinned toolchains and gates. A local Go smoke pass does not
qualify the Julia native runtime or permit bypassing its advisory check.

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
| `make lint` | go vet, tracked/untracked gofmt check and golangci-lint |
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
