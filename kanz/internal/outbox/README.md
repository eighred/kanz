# Outbox service ownership

Each durable FACT stores `producer_service` in the same transaction as its domain
state. `Enqueue` requires the service explicitly. `Postgres` scopes pending keys,
records, publication/failure marks and age metrics to that service. Replicas of
one service retain the same per-table, per-tenant, per-partition drain lock. Tenant RLS still
governs the storage scope; the envelope tenant remains a separate wire identity.

Service ownership prevents one service's relay from publishing another service's
rows through a compatible shared table (#1325). It does not authorize a deployment
to combine service databases or replace domain transaction boundaries.

## Upgrade an existing database

The new ownership migrations are OMS `0025`, accounting `0021`, and datamaster
`0006`. The gaps reserve the versions already assigned in draft #1317; they do
not activate that draft or require its capital tables. The runner tracks each
version individually, so later delivery of those independent migrations must
retain their original numbers and checksums.

1. Preserve the admission halt and verify the database backup/recovery posture.
   Stop writers and relays using the affected outbox before applying the migration.
   Do not run old relays beside new ones: old binaries do not filter by service.
2. Apply the owning service's new migration through `kanz-migrate`. Existing rows
   retain NULL ownership. The `NOT VALID` constraint checks every new/updated row,
   so an old writer cannot add another unowned FACT. It deliberately preserves
   historical rows without inventing provenance.
3. Under each authorized tenant scope, inventory pending rows whose
   `producer_service IS NULL`. New enqueue and drain operations refuse while any
   such row exists in that tenant. This is an explicit reconciliation block, not
   an empty or healthy queue.
4. Reconcile each pending row against original domain transaction and producer
   evidence. Record the reviewed row identities, evidence and decision in the
   operational issue. Assign only the independently established owner in a
   controlled transaction. Do not infer ownership from a subject prefix, payload
   domain, database name or the service applying the migration. If the evidence
   is unavailable, keep the rows unowned and the affected writers/relays stopped.
5. Start only the updated binaries. Verify each service sees and drains its own
   rows, the oldest-pending metric recovers, and committed domain FACTs reach the
   broker with the expected source and tenant. This does not lift capital admission
   or satisfy separate production authorities such as #1316.

A known owner is immutable in PostgreSQL. NULL published history may remain
unclassified; it does not block new work. Do not validate the constraint over that
history or bulk-fill an owner merely to make validation pass. Application rollback
to a binary without service ownership requires a separately reviewed recovery plan;
dropping the constraint to permit old writers would recreate the ambiguity.

## Verification

Use disposable PostgreSQL with a NOSUPERUSER NOBYPASSRLS role and real JetStream:

```sh
go test -race -p 1 ./internal/outbox -count=1 -v
```

Set `TEST_POSTGRES_URL` and `TEST_NATS_URL` for that run. Confirm
`TestPostgresOutboxRejectsCrossServiceOwnership`,
`TestPostgresOutboxScopesEveryOperationToService` and
`TestPostgresOutboxLegacyOwnershipBlocksStateCommitAndDrain` actually execute.
Also run `TestPostgresOutboxPhysicalTablesDoNotShareRowsOrDrainLocks` to verify
that distinct schemas do not alias either records or advisory locks.
They cover foreign-relay publication refusal, scoped mutation and age metrics,
tenant isolation, same-service replica order, restart, immutable known ownership,
and rollback while legacy pending ownership is unknown. No test establishes the
owner of an actual production legacy row.
