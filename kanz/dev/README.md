# Local dev stack (DEVX-01a)

One command to a running platform: NATS spine + Kafka + Postgres + risk-engine +
api-gateway, with a seeded portfolio.

## Quick start

```sh
make generate      # regenerate the schema SDK first (once, or after proto changes)
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
