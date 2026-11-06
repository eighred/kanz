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
the duplicate-fill path does not authorize or write fee corrections. Legacy claims without a journal
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

Unported: admission, venue calls, orchestration, the Store Outbox/Proposals owners, `CommitRecovery` preparation/orchestration and its wider
transaction, fee approval/proposal and full recovery lifecycle, outbox relay, migrations, production
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

Fee-journal compatibility prerequisites are separate from fee posting. `FillWire`
uses the canonical descriptor set and generated Fill for known-field semantics,
and retains unknown fields recursively. Go's decoder normalizes an unknown outer
tag but retains its value bytes; equality groups those bytes by field number,
preserving occurrence order for the same number. Journal encoding emits known
fields deterministically and retained unknown values at each message end. The
original input bytes remain available separately. Inputs/outputs are capped at
1 MiB; unknown-group parsing is iterative with Go's 10,001-group boundary.
The current reachable Fill descriptor graph must remain singular, without
oneofs/maps/extensions or floating-point fields; unsupported schema drift refuses.

Principal normalization uses tables generated from the pinned Go Unicode package,
not Rust full lowercase. CI regenerates the tables byte-for-byte and compares
normalization of every Unicode scalar with Go through the stamped fixture.
This is compatibility data, not authentication, approval or a second policy.
The fee transaction module uses those helpers for the OMS fee journal. The caller
begins and commits or rolls back its transaction. `cas_order` borrows that
transaction exclusively, applies the shared tenant/RLS checks and canonical order
CAS, and returns an opaque token for fee-head reads, revision append and prepared
outbox insertion. It never opens or commits a separate transaction. Original
execution provenance, approval authentication, checkpoint updates and the final
commit remain the caller's responsibility. Any error after CAS requires the caller
to roll back the entire transaction; a Rust error alone does not undo SQL writes.
Existing CAS upsert semantics are not
proof of an observed trade or authorization.

Fee deltas use exact rational arithmetic (previous fee minus revised fee), with
no Decimal quantization. Economic digests reproduce Go length-prefix hashing,
rational text and UTC event-time formatting. Same-case replay compares all known
and retained unknown protobuf fields before consulting the latest head, preserving
ABA replay. Journal bytes are deterministic, while original execution bytes remain
unchanged. Only the OMS book is writable through this token; other books retain
their existing owners. Fee-head and replay reads refuse oversized stored values
before transferring them to the client.

The actual-process qualification requires independent digest/delta checks and ten
PostgreSQL cases covering replay, conflicts, CAS races, rollback, deferred commit
failure, tenant scope, other-book separation and corrupt heads. Local skips do not
satisfy this evidence. This source primitive adds no production caller, approval
authority, recovery orchestration or exclusive writer handoff.


`commit_prepared_recovery` ports only the durable tail of Go `CommitRecovery`.
The existing Go owner still establishes complete venue history, verified account
mapping, posted-history completeness, dual-control approval, terminal-disposition
folding and the once-rounded complete average. This internal prepared batch is
not a public transport or authorization capability. It cannot be used as a claim
that full recovery or its approval/admission path has migrated.

The Rust tail requires exactly one typed recovered FACT for each prepared
execution and one typed status FACT. Tenant, complete raw state, event time,
case/provenance, execution digest and checkpoint must agree. The supplied posted
and missing sets must partition those executions. It binds the stored frozen
history digest/mapping and updates the matching case metadata/version only from
investigating/checkpoint zero. All missing claims are ordered by raw fill ID,
then journaled; order CAS, fee revisions, case CAS, targets and mandatory outbox
records share one transaction. Empty histories become corrected with zero targets.
Normal recoveries remain investigating until the existing two-book ACK owner
confirms completion. The ACK owner is not ported here.

This port caps a prepared batch at 8,000 executions and 4 MiB of state/FACT/posted
bytes, with 1 MiB per state or FACT. These are explicit refusal limits, not a
claim that every batch Go can allocate is accepted. No prefix commits on an
oversized or malformed batch. Nested canonical state/FACT bytes are retained;
ambiguous duplicate singular protobuf fields/groups remain explicitly refused.
Source tests compare the entire changed durable projection with Go CommitRecovery,
including terminal rounded-average and approved-fee cases, then inject missing
facts, CAS races, target/outbox failures and deferred commit rejection. Actual
Linux PostgreSQL evidence is required; local dependency skips do not qualify it.
