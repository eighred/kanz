# Onboarding

Get from a fresh clone to a running, tested platform. The `make` targets
(DEVX-01e) replace the hand-run buf-generate flow the team kept rediscovering —
`make help` lists everything.

## Prerequisites

- Go (see `go.mod` for the version), Docker, and [buf](https://buf.build/docs/installation).
- The repo expects `kanz/` and `kanz-schemas/` as sibling directories (the Go
  module `replace`s the schema SDK to `../kanz-schemas/gen/go`, EVT-15a).

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
anything builds. `buf generate` needs `protoc-gen-go-grpc` on `PATH` (it emits
the inference + query gRPC stubs that `protoc-gen-go` alone omits) — `make
bootstrap` installs it. The module pins `GOFLAGS=-mod=mod` so a stale SDK is
regenerated rather than failing the build.

## The local stack

```sh
make up            # NATS + Kafka + Postgres + risk-engine + api-gateway + seed
curl localhost:8080/v1/portfolios/PF1/exposure
make down
```

Or `tilt up` for a live dashboard. See `dev/README.md`.

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
- Schemas: the sibling `kanz-schemas/` repo (proto + buf); SDKs generate into `gen/`.
- Infra: `infra/` (k8s, GitOps, security, observability, lakehouse, catalog, backstage).
- Runbooks: `docs/runbooks/`. Portal: `infra/backstage`.
