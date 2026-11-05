# Valkey compatibility through the Redis protocol

`Client` remains the production `bus.RedisClient` adapter using go-redis. The
`redis` build tag, `redis://` URLs and existing `*_REDIS_URL` configuration names
describe the protocol and remain stable. CI runs this adapter, webhook nonce
claims and gateway idempotency against Valkey 9.1.3, pinned by image digest.
The server identity test checks `server_name` and `valkey_version` from `INFO
server`; Redis-compatible `redis_version` alone does not identify Valkey.

For an isolated local test dependency, run:

```sh
docker run --rm -p 127.0.0.1:6379:6379 valkey/valkey:9.1.3-alpine@sha256:bf09d58e96d6b4524a15396b65b242bbe18beebbcc15a04d8ee75ddac62fae0b
```

From `kanz/`, with that container running:

```sh
export TEST_REDIS_URL=redis://localhost:6379/0 TEST_VALKEY_VERSION=9.1.3
set -o pipefail
go test -tags redis -p 1 -json -count=1 -run "$(python ../.github/scripts/check_valkey_evidence.py --pattern)" ./pkg/redisadapter ./services/webhook-ingest/internal/ingest ./services/api-gateway/internal/middleware | tee valkey-proof.jsonl
python ../.github/scripts/check_valkey_evidence.py valkey-proof.jsonl
```

The checker rejects skips, failed tests, missing cases and incomplete package
results. Linux CI also runs these cases with `-race`, alongside the existing
full tagged suites. Tests use unique keys and never flush the database. Missing
local dependencies skip integration tests; that is not compatibility evidence.

This changes the CI dependency only. It does not migrate production data,
deployments, persistence, credentials or high-availability configuration. Shared
dedup remains an optimization: database idempotency and financial admission
remain authoritative. Bus dedup and the gateway RedisDedup seam retain their
existing fail-open behavior on backend errors; webhook nonce claims retain
their fail-closed behavior. This compatibility proof does not change those policies.

Version provenance: [Valkey 9.1.3 release](https://github.com/valkey-io/valkey/releases/tag/9.1.3).
