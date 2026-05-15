# Schema Registry

Runtime resolver for `payload_schema_ref` on every Kanz event envelope — see
`kanz-schemas/docs/schema-evolution.md` §6, §8. Producers must resolve their
schema here before publishing; replay (EVT-20) resolves the historical ref
each event was written against.

## Status

**EVT-16a + EVT-16b + EVT-16c** — service skeleton, Postgres/in-memory
storage backend, ingest endpoint that kanz-schemas's release CI calls on
every tag, and `resolve(payload_schema_ref)` for runtime consumers.

## HTTP

| Route | Purpose |
|---|---|
| `GET  /healthz` | liveness |
| `GET  /readyz` | readiness — pings storage |
| `POST /schemas/{schema_id}` | ingest a FileDescriptorSet subset. Body is raw bytes; `X-Source-Tag` header carries the kanz-schemas release tag. Registry assigns the version: same fingerprint as the latest → `200 OK` (no version bump); new content → `201 Created`. Response body: `{"ref":"<id>:<version>","created":<bool>}` |
| `GET  /schemas/{ref}` | resolve a `payload_schema_ref` (e.g. `market.v1.MarketDataEvent:7`). Body is the raw stored descriptor; metadata is in headers: `X-Schema-Fingerprint`, `X-Schema-Source-Tag`, `X-Schema-Created-At`, `ETag`. The row is immutable, so the response is sent with `Cache-Control: public, max-age=31536000, immutable`. Unknown ref → `404`; malformed ref → `400` |

## Layout

| Path | Purpose |
|---|---|
| `cmd/schema-registry/` | binary entrypoint |
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

```sh
psql "$SCHEMA_REGISTRY_DATABASE_URL" -f migrations/0001_init.sql
go run ./cmd/schema-registry
```

## Tests

`go test ./...` runs the in-memory backend tests by default. The Postgres
backend tests run only when `TEST_POSTGRES_DSN` is set against a database
with `migrations/0001_init.sql` already applied.
