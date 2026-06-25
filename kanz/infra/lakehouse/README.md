# Lakehouse research access (LAKE-01d)

Query engine + notebook environment over the analytical store the `lake-sink`
service (LAKE-01a) lands. Closes the loop the lakehouse epic opened: the event
log is now streamed to durable storage **and** explorable for research,
backtesting (LAKE-01c), and dataset building (LAKE-01b).

## Topology

```
Kafka log ──▶ lake-sink ──▶ object store           Hive Metastore
 (EVT-09)     (LAKE-01a)    {domain}/{entity}/dt=…   (table + partition defs)
                            part-*.ndjson                 │
                                  │                        │
                                  ▼                        ▼
                              Trino (coordinator + workers) ──▶ Jupyter notebooks
                                  hive catalog  (raw NDJSON)        (trino client,
                                  iceberg catalog (promoted)         %sql, backtest)
```

| File | What |
|---|---|
| `trino/catalog-hive.properties` | Hive connector over the raw lake-sink NDJSON landing — always-available, queryable the moment events land |
| `trino/catalog-iceberg.properties` | Iceberg connector for promoted/compacted tables (columnar, snapshot isolation, time-travel) |
| `trino/trino.yaml` | Trino coordinator + worker pool, restricted-PSS namespace `kanz-lakehouse`, catalogs mounted from a ConfigMap |
| `notebooks/jupyter.yaml` | Jupyter pod pre-wired with the Trino python client for interactive research |
| `notebooks/example-queries.sql` | External-table DDL + sample queries against the landing |

## Read-only by posture

Research must never mutate the log of record. Both catalogs disable managed
writes (`hive.non-managed-table-writes-enabled=false`); the workloads run
non-root, read-only-rootfs, under the restricted Pod Security Standard (SEC-02a),
isolated in `kanz-lakehouse` so a runaway analytical query can't starve
`kanz-services`. Object-store and notebook credentials come from secret mounts
(SEC-01d), never inline.

## Raw vs promoted

The Hive catalog reads the NDJSON landing directly, so there is zero lag between
an event landing and being queryable — and the columns evolve automatically,
since `lake-sink` decodes payloads against the EVT-16 registry. A separate
compaction job (rewrite NDJSON → Iceberg) gives the columnar performance,
snapshot isolation, and `FOR VERSION AS OF` time-travel that production research
wants; those tables are served by the Iceberg catalog. The two are complementary:
the Hive view is the live raw tap, the Iceberg view is the curated store.

## Backtesting from a notebook

A notebook drives the `tools/backtest` harness (LAKE-01c) over a replay range and
the LAKE-01b point-in-time materializer, so a strategy evaluated on historical
events reproduces the live decision stream bit-for-bit (the determinism +
no-future-leakage properties tested in `tools/backtest`). Trino + Iceberg
time-travel complements the bitemporal `AsOf` reads for ad-hoc "what was knowable
at T" analysis.
