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

## Recovery acknowledgement transaction

`recovery_ack::acknowledge_recovery` ports the durable acknowledgement tail of
Go `order.Postgres.AcknowledgeRecovery`. The trusted caller supplies the canonical
`ExecutionRecoveryApplied`, a closed `Book::{Ledger,Position}` enum and one
mandatory typed corrected-status FACT. This is not an acknowledgement admission
API: the existing Go `HandleRecoveryAck` retains tenant/FACT checks and derives
book identity from the authenticated subject. A string in a payload must never
select the book being acknowledged. No production caller, listener, DSN or
transport switch is added.

The transaction locks the tenant's case, verifies the exact execution target and
payload/execution digests, inserts the idempotent book acknowledgement and closes
the case only once the complete expected count is reached. Completion binds the
prepared status FACT to the stored case and order and commits its outbox row with
the acknowledgement and case update. The immutable target/execution evidence and
order bytes remain unchanged. Replays of an already corrected case still require
the matching target, and cannot emit a second corrected FACT. Stored case strings
and order bytes are size-checked by SQL before transfer; missing, malformed or
oversized stored data is an error, never a partial read or success. Shared scoped
transactions require NOSUPERUSER/NOBYPASSRLS and forced tenant RLS.

`CommitUnconfirmed` retains its existing meaning: transport failure around COMMIT
is not proof of rollback. Reconcile the durable case, acknowledgement and outbox
before retry; a deferred-trigger rollback test proves only its injected rejection.
The fixed disposable fixture mode and independent Go/PostgreSQL comparisons cover
both-book completion, duplicates, concurrent final acknowledgements, restart,
tenant isolation, required FACT/target binding, corrupt storage and outbox/commit
rollback. Linux actual-process evidence is mandatory; locally skipped dependency
tests are not acceptance. The source-only migration still lacks recovery/admission
orchestration, production writer fencing, approved handoff and runtime activation.

Corruption refusal is intentionally stricter than the current Go acknowledgement
owner: Rust checks `expected_ack_count = 2 * checkpoint` and the stored target
count equals the checkpoint before inserting an acknowledgement. Go currently
can commit a pending acknowledgement with a zero expected count or extra targets.
The real differential fixture preserves this distinction explicitly; it does not
claim malformed stored metadata is compatible or change Go production behavior.

## OMS queue repository

`outbox` implements the six existing Go Queue persistence operations over the
same `oms` rows and canonical raw Record. Rust Create/Save/recovery enqueue into
this table; the production Go relay, admission and publisher remain unchanged.
This source port does not make Rust an authoritative running OMS.

The session lock uses Go's database/table OID/tenant/service/key namespace.
Its trusted connector must transfer both the Client and its own driver task to
Session and supply the same connector TLS policy for protocol CancelRequest.
Cancellation/drop dispatches a bounded CancelRequest and aborts the driver;
closing a socket alone cannot interrupt a PostgreSQL backend waiting for a lock.
Only verified unlock returns the session. No failed unlock can return a connection
to a pool. Caller acquisition deadlines can be shorter; the server lock wait has
a five-second ceiling so failed cancellation/shutdown cannot strand a backend.
This is an explicit bounded refusal relative to Go's caller-context-only wait.
Release also has a five-second deadline.

Explicit compatibility limits: pages above 4096 rows or 1 MiB of combined dynamic
fields refuse rather than return a prefix. Nonpositive limits retain Go's empty
success. Failed-cause truncation retains the first 1000 bytes; a split UTF-8 scalar
is refused, matching PostgreSQL's rejection of Go's resulting invalid string.
The signed schema-version bits remain unchanged in Record. Missing ownership and
unsafe RLS/roles refuse. Age distinguishes empty from present and saturates at Go's
maximum duration. Mark operations use tenant-local transactions; failed COMMIT is
CommitUnconfirmed, never a general rollback claim. Go's corresponding autocommit
error is likewise not proof of rollback; retry keeps at-least-once semantics.

Real Go/Rust PostgreSQL contention, cancellation, session-loss, raw-byte and mark
proof is mandatory in `TestRustOutboxQueue`; local skipped tests do not qualify it.

## Held-order proposal repository

`proposals` ports all seven existing OMS ProposalStore persistence operations.
The original canonical SubmitOrder bytes and row metadata remain in
`order_proposals`; an unapproved proposal never becomes an order. Put, Claim and
AnnounceExpiry commit their markers and existing OMS outbox Records together.
Claim requires a FACT; Put and AnnounceExpiry retain Go's permitted empty batch.
RecordRefusal changes only the latest refusal on an undecided row. Decisions and
expiry announcements retain their rows and exclude each other atomically.

This is a repository owner, not the maker/checker authority. The existing Go
caller authenticates the issuer, checks `dualcontrol.Approve` against the captured
time and digest, then replays the stored command through the complete admission
path with `approval.Covers`. Rust keeps the original caller-time SQL predicates;
it adds no DB-clock expiry policy. SQL lower/btrim remains the final self-approval
comparison. The separately recorded delayed-admission timing boundary under
#1413/#796 is not repaired or qualified by this port.

The trusted caller supplies an already connected NOSUPERUSER/NOBYPASSRLS client;
forced RLS on the proposal table is required in addition to existing store tables.
Raw commands retain unknown fields. Reads decode corruption but, like Go, do not
re-run Put validation on stored metadata. Nullable decision/refusal/expiry times
remain distinct from missing rows. Error or capacity refusal returns no prefix.
The explicit port bounds are 4096 rows, 1 MiB total dynamic bytes per operation
and 64 announcement records. Expired pages retain Go's default100 on nonpositive
limits; over-bound explicit limits refuse. Pending is a complete bounded result,
not a silently truncated queue. COMMIT errors remain unconfirmed outcomes.

The actual Go `proposalstoretest.Run` is reused against the subprocess adapter;
the adapter does not validate requests before Rust. Separate real-PostgreSQL
fixtures compare Go readback, concurrent claim/expiry arbitration, exact raw
commands and nullable metadata, caller-time boundaries, RLS, whole-result bounds
and transaction/outbox rollback. The adapter's synthetic approval FACT is built
independently of the actor under test so invalid actors reach the repository.
No production caller, service, transport, writer handoff or credential owner is
introduced; actual Linux no-skip qualification remains required.

## Outbox relay qualification

`relay` composes the existing OMS-owned Queue with a supplied acknowledged
publisher. It introduces no production connector, deployment or Go handoff.
`Run`, `DrainOnce` and synchronous `Flush` retain per-key advisory ownership,
read-after-lock ordering, stop-at-first-failure, background progress on other
keys and the full-batch Flush stall. Broker acknowledgement precedes
MarkPublished. Acknowledgement followed by an unconfirmed database result leaves
at-least-once redelivery possible; it is never proof that publication rolled back.
Go Record.Event does not persist EventID, so each retry receives a fresh UUIDv7
and idempotency key while durable payload, timestamp and attribution survive.

Canonical descriptors resolve payload types. Known protobuf fields are normalized
like Go Unmarshal/Marshal, including duplicate embedded messages. The existing
fee-wire parser preserves unknown values and group bytes, with canonical tags;
unknown bytes are reattached in encounter order at each message level. Independent
Go byte comparisons are required, not semantic equality alone. Reachable payloads
include OMS order/approval/outcome/attribution/recovery FACTs and the shared OMS
position projector's domain.v1.PositionState. Current graphs have no maps or
oneofs; future such fields refuse pending overwrite-semantics qualification.

Explicit limits are 1 MiB record/frame bytes, 32 known-message nesting levels,
4096 sequence entries (8 MiB total key bytes) and queue rows, a one-hour
sequence TTL, and ten seconds for
each broker acknowledgement. Sequence eviction re-baselines at one, like Go;
its smaller ceiling is a declared port bound. Only FACT class is accepted.
Missing schema/required envelope fields or malformed payload refuses before the
publisher. Errors retain the distinction between a key stalled by publication
and infrastructure/commit failure. The fixture records fixed failure categories,
not unbounded broker diagnostics or credentials.

Stop prevents starting another record; an in-flight acknowledged publication is
joined through its mark/unlock before returning. Dedicated session close awaits
cancellation/driver teardown, and no session goes back to a pool. The trusted
connector must bound connection and SQL operations; the disposable fixture sets
a five-second SQL statement timeout. Abruptly dropping the owner still has the
previous bounded cancellation fallback, not a graceful-shutdown guarantee.

`examples/relay_support` alone connects to a loopback disposable JetStream broker.
The required real PostgreSQL/NATS fixture compares canonical Go framing, ordered
draining, contention, poison keys, mark failures, ACK/SQL ambiguity, restart and
joined stop. This proves neither production SPIFFE/ACL acceptance nor permission
to replace the Go relay. prost-reflect 0.16.2 declares prost/prost-types ^0.14.0;
the lock retains the existing exact 0.14.1 versions and advisory checks.

## Position book qualification

`position::apply` and `position::snapshot` implement the existing Go position
Store boundary against its original tables. The instrument advisory lock, fill
claim, chronological history/basis, venue lot, cross-venue aggregate, position
fee journal and Announcer records share one transaction. The synchronous
Announcer only constructs Records; it cannot publish or perform I/O. Duplicate
fills still announce the current book. Failed announcements roll back claims,
history and positions; a failed COMMIT remains unconfirmed. The existing outbox
owner supplies drainage. No Go caller is switched to Rust.

The arithmetic is exact weighted-average cost and **gross** realized P&L. Fees
remain separate journal revisions, matching the current OMS reporting convention.
The snapshot is cost-valued, not a live valuation: cash and log coordinates remain
absent. It is neither evidence of financial genesis nor a complete bootstrap
snapshot. Missing price retains the legacy store conversion semantics; upstream
fill/admission validation is not replaced by this compatibility owner.

The bounded domain accepts canonical Decimal exponents through ±64 and refuses
unrepresentable output. Stored rational text must be Go RatString integer or
numerator/positive-denominator form, at most 4096 bytes per field; arithmetic
intermediates are limited to 16384 bits. Rows/replay are capped at 10000, with
1MiB aggregate transfer/result budgets and 64 Announcer Records. SQL window
checks withhold oversized data before transfer. Overflow refuses the entire
operation/result, never a prefix or fabricated zero. Incomplete legacy history
cannot accept backdated or recovery executions. These are explicit bounded
refusals, not a claim of arbitrary big.Rat input compatibility.

Qualification must compare actual Go/Rust PostgreSQL state and raw journals,
independent rational goldens, cross-writer contention and rollback, recovery/fee
history, RLS and actual NATS publication ordering. Local Windows formatting or
skipped tests do not establish this acceptance. Production admission, credentials,
venue gRPC, halt, DR and single-writer cutover remain separately governed.
