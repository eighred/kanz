# Recovery observation coordinator

This source-first owner composes the existing order aggregate, OMS repository,
fee journal, typed venue client and relay for three Go handler contracts:
`HandleRecovery`, `HandleRecoveryAck` and `HandleRecoveryParked`. It has no
production caller. Go remains the authoritative OMS; this library neither
executes nor cancels orders, redrives orphans, resumes trading, creates fee
approvals, nor replaces risk/compliance/mandate admission.

Observation is an investigation trigger. The configured portfolio/account
binding must match the stored order and an actual SPIFFE mTLS `Describe` result;
`QueryOrder` supplies independent executed quantity and complete history.
A durable blocked case can return handler success after its FACT is published,
matching Go. That response does not mean the financial books recovered. Missing
or unavailable evidence cannot become a healthy empty history.
Persisted maker/checker, digest or expiry policy refusals return
`ApprovalRefused`; they do not manufacture a durable blocked acknowledgement.
An unmatched order mapping or inconsistent frozen-history digest returns
`RecoveryEvidenceConflict`, preserving Go's separate refusal path.

Immutable original observation bytes and their hash are retained. The existing
Go observation, Query history and frozen-history contracts permit 4 MiB, with
at most 8,000 accepted fills. Query decoding may retain one extra count sentinel
so 8,001 fills reach the existing durable blocked decision rather than accepting
a prefix. Ordinary canonical decoding and other venue RPCs remain 1 MiB.
Generated known values and recursive unknown wire are preserved by the shared
canonical carrier; these byte limits are not a claim of 4 MiB resident memory.

Financial preparation checks complete posted claims/journal, tenant attribution,
immutable mapping/history, exact quantities, once-rounded complete average and
persisted fee proposal/approval identity, digest, history, maker/checker and
expiry. Existing Go caller-time conventions remain. Posted journal rows are
read through bounded one-row keyset queries in one read-only repeatable snapshot;
the final transaction rereads fee heads under the order CAS lock. The shared
transaction tail commits state, claims, journal, fee revisions, targets and all
FACTs atomically. Each concrete record is materialized separately, with no
financial commit batching. Any late serialization, target, outbox or SQL error
rolls back that transaction; earlier observation/mapping/frozen-history commits
are not rolled back or misrepresented as absent.

An individual emitted FACT remains bounded to 1 MiB. The Go journal owner also
invokes DecodeRecovery before its financial CAS and rejects recovered payloads
larger than 1 MiB; this refusal is verified with unchanged financial tables, not
misclassified as a postcommit broker failure. Rust additionally bounds complete
record/frame metadata, so this is not universal wire/operational parity. Broker
maximum payload and stream size are read back
in the real fixture. `CommitUnconfirmed` does not prove rollback, and a relay
stall (including a full bounded batch) returns `PublicationUnconfirmed` while
preserving committed pending records for at-least-once retry. Separate position
and ledger ACKs must match current targets before a case becomes corrected.

The coordinator creates no detached financial tasks. The caller serializes its
mutable owner and must join Handle before Close. Dedicated SQL sessions close
before relay publication. Explicit command timeout closes/joins those sessions;
dropping the whole future retains the dependency owners' cancellation fallback,
which is not the same as awaiting explicit close. Venue Close joins its owned
transport and identity tasks. The fixture observes SQL/WAPI/gRPC closure while
the process is still alive, rather than relying on process exit.

`examples/recovery-coordinator-fixture.rs` is only a disposable qualification
adapter. It accepts loopback test databases/broker and an authenticated synthetic
venue, inherits only explicit test environment, emits closed status codes, and
never prints DSNs, financial payloads or raw SQL errors. Its bounded JSON is not
a production service boundary. It has a 15-minute lifetime and per-command
30-second absolute work budget across connection, handler and relay; it does not
batch ACKs. That clock continues while the handler session is closed, and no
relay phase starts after expiry. Deadline is not a rollback receipt: a commit or
broker acknowledgement can already have occurred. Post-handler/relay expiry is
publication-unconfirmed; only broker-acknowledged and SQL-marked records count as
a confirmed prefix, and the unresolved suffix is retried idempotently.

Cleanup is separately awaited: a held relay lock closes its dedicated session
on expiry (up to 5s protocol cancellation grace); normal release allows up to 5s
unlock plus up to 5s cancellation before aborting/joining the owned driver.
The fixture then allows 3s to observe SQL session absence. Its 45s response wait
observes the 30s work budget plus this cleanup; it does not extend financial work
and is not a hard operating-system scheduling bound. Cancellation before a
connector returns its Session still uses the connector's abort-on-drop fallback,
not a claim of explicitly joined pre-Session shutdown. Existing general relay
APIs retain their semantics; the coordinator uses the owned deadline variant.

Actual runtime qualification uses the optimized release binary with workspace overflow checks enabled; debug unit tests
and Clippy remain separate. Each response reports its compiled assertion profile,
and evidence binds the immutable source, release target path and executable hash.
The earlier debug-profile 8,000-fill Deadline remains a failed result, not release
performance evidence.

The separate `recovery-coordinator` job in the existing `rust-oms-fold` workflow
retains every original seven-package OMS proof. Its independent Go oracle runs
actual PostgreSQL, NATS and synthetic rotating SPIFFE mTLS. Large Go and Rust
8,000-fill cases run separately and exercise all 16,000 book ACKs. The dedicated
primary PostgreSQL fixture explicitly provisions `max_locks_per_transaction=1024`,
`max_connections=100`, `max_prepared_transactions=0`; a second unchanged-default
64/100/0 instance records actual complete commit or independently identified
resource refusal. This shared setting is not a per-transaction lock ceiling. A
third isolated 10/100/0 server requires actual SQLSTATE53200 (out of memory) and complete financial
rollback, retaining connection availability and post-error database usability.
The closed DatabaseMemory error identifies that SQLSTATE, not an inferred cause
for every query. This fixture observes it under the lock-heavy atomic transaction.
All use NOSUPERUSER/NOBYPASSRLS and twelve FORCE-RLS owner tables. This is disposable
capacity qualification, not production tuning or a concurrency guarantee.
Observed process RSS is retained as measurement, not an accepted resource ceiling.
The source/tree/run/attempt-bound artifact requires every named test, both large
streams, complete package outcomes and zero skips through the shared verifier.

Production adoption still requires authenticated incoming consumer ownership,
startup/DR/halt/fencing and full service shutdown, funded lifecycle and old-writer
compatibility from [#1316](https://github.com/eighred/kanz/issues/1316), explicit
capacity/concurrency acceptance, recovery/rollback and exclusive writer handoff.
[#1413](https://github.com/eighred/kanz/issues/1413) owns those decisions. A compiled
fixture or compatibility pass does not activate recovery or close that issue.

Qualification-only stage diagnostics
-----------------------------------

The off-by-default `qualification-diagnostics` feature records fixed-size,
saturating counters and nanoseconds in a task-local command scope. It changes
no SQL, work budget, session lifetime, ACK count or retry decision. The release
fixture reports its actual feature identity in each existing private response;
the Go harness requires that identity and retains aggregates only in memory.
Only existing worker transport/response failure diagnostics emit these arrays;
typed command errors reported directly by their callers do not necessarily emit
a profile. Passing runs do not
publish stage measurements. A failed command without a complete response is
excluded: the output explicitly describes completed-command aggregates, never
the current stage or an observed cause of the failure.

Array order is HandlerConnect, LoadCase, LoadOrder, DurableAck, AckTargets,
AckReceived, HandlerClose, RelayConnect, RelayAcquire, RelayPending,
RelayRelease, RelayClose, FixtureAbsence, Total. Durations are nonadditive:
AckTargets and AckReceived are nested in DurableAck; Total includes the other
stages. Counts describe executed stage scopes, not successful financial effects.
The 900-second fixture lifetime, 30-second work budget, all 16,000 ACKs and
explicit cleanup checks remain mandatory. These observations alone do not
establish a performance bottleneck or production performance acceptance.

ACK context reads share one explicit READ COMMITTED, read-only transaction.
The standalone case/order APIs retain their completion behavior; the paired ACK
read has one rollback after both reads instead of an intermediate case COMMIT
and a second scope. Case validation still precedes order validation. Role checks
remain first; tenant configuration and the unchanged five-table FORCE RLS count
share a catalog-only SELECT, and the count must pass before application SQL.
The final locked ACK/target/count/CAS/outbox transaction and all sessions, joins
and work limits remain unchanged. LoadCase/LoadOrder diagnostic scopes now cover
only their reused row reads; shared read admission/rollback is included in Total,
so these stage scopes are not comparable with the preceding diagnostic revision.
Fresh full dependency qualification is required; no latency improvement follows
from source round-trip counts alone.
