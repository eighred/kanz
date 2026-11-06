# OMS repository transaction port

This library implements the existing OMS `Create`, `Save`, `Load` and
`AppliedFills` repository operations using
Rust and the canonical Go migrations. It has no production caller, connection
factory, configuration or network listener. The diagnostic fold worker remains
database-free. [Migration #1413](https://github.com/eighred/kanz/issues/1413)
owns the later authority decision.

The caller supplies an admitted canonical state, expected version and prepared
FACTs. The library is a trusted repository, not an authentication or approval
boundary. It stores the original state and FACT bytes, including unknown fields
and exact Decimal representations. The nested journal fill is retained byte for
byte. Ambiguous repeated nested fill fields and protobuf groups are refused;
this port is qualified for canonical producer encodings, not every wire encoding
accepted by Go's protobuf decoder.

Every save uses a transaction-local tenant. Connections with SUPERUSER or
BYPASSRLS, or missing FORCE RLS on any of the five durable tables, refuse.
The claim, execution journal, CAS and outbox commit together. Missing rows retain
the existing tenant-local insert-at-version-zero behavior; an expected version
does not turn an absent row into a cross-tenant identity check. Storage tenant
and envelope tenant remain distinct, as in the Go repository.

`Create` inserts once and commits its prepared FACTs atomically. Duplicate
creation returns `Exists` without changing the row or adding announcements.
This is storage admission atomicity, not risk/compliance/mandate permission.
`Load` returns original state bytes and version from one SQL row; `NotFound`
is distinct from unavailable/corrupt data. `AppliedFills` returns the complete
set of raw legacy fill aliases where present, otherwise stored claim keys.
Claims may only suppress replay, never authorize it. Empty results are produced
only after successful scoped reads (or an explicitly empty order identifier).

All operations reuse tenant/role/RLS transaction setup; both writers reuse
payload validation, outbox insertion and commit handling. Reads roll back their
read transaction before returning success. SQL conditional projections prevent
over-limit BYTEA/TEXT values from being transferred into the client: state has a
1 MiB byte bound, and claims have a 100,000-row and 1 MiB aggregate text bound.
Exceeding either returns `Capacity`, never a truncated claim set. These are
explicit bounded-port limits, not a claim that Go rejects every same-sized read.
Result-object overhead is additionally bounded by row count; no heap profiling
or latency qualification is claimed. A mid-read PostgreSQL error discards the
entire claim result, including any rows evaluated before the failure.

Scoped execution keys reproduce Go JSON escaping before SHA256. Duplicate
economic equality uses canonical exact Decimal comparison, presence and venue
time. It recognizes the original execution or the latest validated fee head;
it does not authorize or write fee corrections. Legacy claims without a journal
retain their existing treatment. Unowned pending outbox rows block new FACTs.

`CommitUnconfirmed` means the commit did not return confirmation. It does not
prove rollback; a canceled call or connection loss around COMMIT requires durable
reconciliation before retry. The fixture's deferred failure proves rollback for
that injected server-side rejection only. All saves currently use a transaction,
including announcement-free saves, to contain tenant scope; latency has not been
qualified and no performance equivalence is claimed.

`examples/fixture.rs` is a test adapter with bounded stdin/stdout, a fixed timeout
and a compile-time source stamp. It permits only loopback PostgreSQL hosts,
rejects hostaddr overrides, and uses isolated fixture schemas. Its JSON is not a
production service contract. No application environment is inherited by the Go
launcher beyond the disposable test DSN. No secrets, SQL errors or payloads are
printed by the Rust adapter.

From the repository root, using a disposable migrated non-bypass PostgreSQL role:

```sh
KANZ_BUILD_VERSION="$SOURCE_SHA" cargo +1.90.0 build --locked --manifest-path kanz-rs/Cargo.toml -p kanz-oms-store --example fixture
cargo +1.90.0 test --locked --manifest-path kanz-rs/Cargo.toml -p kanz-oms-store --all-targets
cd kanz
KANZ_OMS_STORE_BIN="$ABSOLUTE_FIXTURE_BINARY" KANZ_OMS_STORE_VERSION="$SOURCE_SHA" \
  go test -race -p 1 ./services/oms/tools/store-conformance -count=1 -json
```

Set `TEST_POSTGRES_URL` only to that disposable database. Missing dependencies
explicitly skip locally; the existing OMS workflow and evidence checker require
every actual Rust-write case without skips. Go independently checks durable
state, raw bytes, versions, claims, journal and outbox. Restart tests restart the
client process/pool, not the PostgreSQL server.

Unported: admission, venue calls, orchestration, the Store Outbox/Proposals owners, `CommitRecovery` and its wider
transaction, fee approval/write lifecycle, outbox relay, migrations, production
fencing and rollout. Deployment and exclusive writer handoff need separate
recovery, rollback, capability and latency proof. This library is not a second
production OMS and does not close the migration.

The query module implements `List`, `ListByStatus`, `ListByParent` and
`ListByPortfolio` using the existing denormalized columns and canonical indexes.
It returns the original protobuf bytes, including unknown fields, after decoding
for validity. General queries refuse more than 100,000 rows or 1 MiB of combined
state bytes; SQL withholds all state bytes if that selection exceeds the byte
bound. Errors never return a successful prefix. Status selectors are bounded to
128 entries; unknown numeric codes retain Go selection semantics.

Portfolio limits clamp to 200 exactly as Go does. Empty parent/portfolio selectors
do not select unattributed orders. Portfolio pages order by `created_at DESC`
without a tie-break guarantee. The unindexed count is a separate READ COMMITTED
statement, matching the actual Go implementation: it is not a simultaneous
snapshot of the page. Queries add no admission or recovery authority. The query
qualification compares actual Rust reads with Go and raw bytes, tenant isolation,
capacity/corruption refusals, and selective plans using existing indexes.
