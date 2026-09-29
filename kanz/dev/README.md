# Local dev stack (DEVX-01a)

Fresh clone to a running, tested platform, then one command to a live loop:
NATS spine + Kafka + Postgres + risk-engine + api-gateway, with a seeded
portfolio. `make help` lists every target.

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
make build test    # build everything, run the suite
```

### Why `make generate` is step one

Generated code is never committed (EVT-15a). The kanz module `replace`s
`kanz-schemas-go` to `../kanz-schemas/gen/go`, so that tree must exist before
anything builds. `buf generate` needs `protoc-gen-go-grpc` on `PATH` — it emits
the inference + query gRPC stubs that `protoc-gen-go` alone omits — and `make
bootstrap` installs it. The module pins `GOFLAGS=-mod=mod` so a stale SDK is
regenerated rather than failing the build.

## Quick start

```sh
make up            # docker compose up + seed
# ... develop ...
make down          # tear down
```

Or with Tilt (live dashboard + log multiplexing):

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
| `make build` / `make test` | build / `go test -race ./...` |
| `make lint` | `go vet` + gofmt check |
| `make up` / `make down` | local dev stack |
| `make scaffold NAME=x` | new service skeleton |
| `make ci` | what CI runs: generate → lint → build → test |

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
