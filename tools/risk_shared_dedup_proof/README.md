# Risk-engine shared deduplication fixture

Manually dispatch `valkey-migration.yml` with `qualification=shared-risk` and
`expected_sha` set to the full reviewed commit. The job builds the service's
`default` and `shared-dedup` Docker targets and runs them by immutable local
image ID. Build and fixture containers run on a disposable GitHub runner.
The default image must reject an explicitly configured shared backend.

The selected images share a loopback-only namespace with Valkey 9.1.3, NATS
2.15.3 and PostgreSQL 16. The broker uses the development configuration and
stream declarations with single-replica, bounded storage. Only the tenant-scoped
`risk_proof` role can connect to the database over TCP. The fixture applies
risk migrations 0001–0005. It creates no production resources.

For the fixed portfolio, shard A receives the first position and acknowledges it
before shard B starts. B must independently consume it and publish the exact
signed and gross exposures. The driver verifies the source revision, synthetic
tenant, typed query result and each shard's durable acknowledgement floor.
Each repeat uses the identical EventFrame and receives a new broker sequence.
While Valkey is available, repeats increment the suppression counter on both
shards and leave handler and recompute counts unchanged.

After a graceful stop the owner must have persisted the position and applied
key under forced row security. Restart must emit a newer exposure from the
checkpoint without another live position dispatch. During a Valkey-only outage,
both clients must report failed claims and process every delivery. Recovery
must suppress repeats again. The last checkpoint must contain the final
quantity, price, market value and all three distinct event identities.
These checks cover graceful recovery of the synthetic book.
