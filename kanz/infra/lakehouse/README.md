# Lakehouse research access

The workload manifest configures one Lake sink to consume the derived Kafka archive and write NDJSON to a ReadWriteOnce volume. The landing path is `/var/lake/landing`. The manifest lists each subscribed topic explicitly. An empty query for an unsubscribed topic cannot establish that no event occurred. [Lake sink deployment](../deploy/lake-sink-deploy.yaml)

## Inspect a finite snapshot

In an environment qualified for finite snapshots, the operator can inspect a tenant's selected events through the authenticated gateway. Creation requires `lake.snapshot.create`. Inspection requires `lake.snapshot.read`. The owner selects matching events from files open in the active landing run. A portfolio-restricted session is refused by v1. See the [Lake sink runbook](../../services/lake_sink/README.md#optional-finite-snapshot-owner) for enablement, identity and storage prerequisites.

Given a snapshot UUID issued by that owner and the operator's configured `KANZ_GATEWAY_URL` and credentials, read its metadata first. Run these commands from the `kanz/` Go module.

```sh
go run ./cmd/kanz_lake_snapshot -operation get -snapshot-id "$SNAPSHOT_ID"
```

For `SNAPSHOT_STATE_READY`, run the queries you need.

```sh
go run ./cmd/kanz_lake_snapshot -operation list -snapshot-id "$SNAPSHOT_ID" -page-size 100
go run ./cmd/kanz_lake_snapshot -operation count -snapshot-id "$SNAPSHOT_ID" -page-size 100
go run ./cmd/kanz_lake_snapshot -operation failures -snapshot-id "$SNAPSHOT_ID" -page-size 100
```

`list` returns selected event identities, types, timestamps, ordinals and decode status. `count` groups the selection by event type. `failures` lists events with decode failures. For a list or failures response with `exhausted=false`, pass the returned `next_ordinal` as `-after-ordinal` on the next call, including when its value is zero. Exhausting the selection establishes the end of that snapshot. Historical completeness requires independent source-range and recovery evidence. [Snapshot contract](../../../schemas/proto/lake/v1/snapshot.proto)

## Reference notebook and catalogs

The GitOps inventory excludes this single-user research reference. The [Jupyter manifest](notebooks/jupyter.yaml) installs a Trino client and points it at the [Trino service](trino/trino.yaml). The [Hive](trino/catalog-hive.properties) and [Iceberg](trino/catalog-iceberg.properties) catalogs expect an S3-compatible store and a Hive metastore. The [example SQL](notebooks/example-queries.sql) includes catalog and partition setup as well as research queries. These dependencies and the transfer from the landing volume require separate provisioning. [GitOps scope](../../test/arch/gitops_sync_completeness_test.go)

Complete source ranges, immutable object-store publication, Lakekeeper commits and verified restore from the first generated records remain under [the Lake migration](https://github.com/eighred/kanz/issues/1540). The [finite Parquet worker](../../../rust/services/lake_snapshot/README.md) supports the bounded inspections above.
