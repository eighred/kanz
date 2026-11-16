# Schema Registry

Runtime resolver for `payload_schema_ref` on every Kanz event envelope — see
`schemas/README.md § Schema Evolution` §6, §8. Producers must resolve their
schema here before publishing; replay (EVT-20) resolves the historical ref
each event was written against.

## Responsibility

The service stores immutable versioned descriptors with PostgreSQL and an
in-memory test backend. Schema release tooling calls its ingest endpoint;
runtime consumers resolve the historical descriptor identified by each event.
It does not own domain state or authorize financial actions.

## HTTP

| Route | Purpose |
|---|---|
| `GET  /healthz` | liveness |
| `GET  /readyz` | readiness — pings storage |
| `POST /schemas/{schema_id}` | ingest a FileDescriptorSet subset. Body is raw bytes; `X-Source-Tag` header carries the schemas release tag. Registry assigns the version: same fingerprint as the latest → `200 OK` (no version bump); new content → `201 Created`. Response body: `{"ref":"<id>:<version>","created":<bool>}` |
| `GET  /schemas/{ref}` | resolve a `payload_schema_ref` (e.g. `market.v1.MarketDataEvent:7`). Body is the raw stored descriptor; metadata is in headers: `X-Schema-Fingerprint`, `X-Schema-Source-Tag`, `X-Schema-Created-At`, `ETag`. The row is immutable, so the response is sent with `Cache-Control: public, max-age=31536000, immutable`. Unknown ref → `404`; malformed ref → `400` |

## Layout

| Path | Purpose |
|---|---|
| `cmd/schema_registry/` | binary entrypoint |
| `internal/server/` | HTTP server, `/healthz` + `/readyz` |
| `internal/storage/` | `Storage` interface + Postgres and in-memory backends |
| `internal/config/` | env-driven config |
| `migrations/` | hand-applied SQL migrations |

## Config

| Env var | Default | Notes |
|---|---|---|
| `SCHEMA_REGISTRY_LISTEN` | `:8080` | HTTP listen address |
| `SCHEMA_REGISTRY_DATABASE_URL` | _required_ | Postgres DSN |
| `SCHEMA_REGISTRY_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

## Run locally

From `kanz/services/schema_registry/`, using a disposable database:

```sh
psql "$SCHEMA_REGISTRY_DATABASE_URL" -f migrations/0001_init.sql
go run ./cmd/schema_registry
```

## Tests

From that same directory, `go test -p 1 ./... -count=1` runs the in-memory backend tests by default. The Postgres
backend tests run only when `TEST_POSTGRES_URL` is set — the same variable
every other DB-gated suite in this repository reads, and the one CI sets. The
suite creates its own Postgres schema and replays `migrations/*.sql` into it,
so the database needs no prior setup beyond the role being able to
`CREATE SCHEMA`.
