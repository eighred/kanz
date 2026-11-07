# Recovery observation coordinator

This source-first owner composes the existing order aggregate, OMS repository,
fee journal, typed venue client and relay for the existing Go handler contracts:
`HandleRecovery`, `HandleRecoveryAck`, `HandleRecoveryParked` and
`HandleFeeCorrection`. It has no
production caller. Go remains the authoritative OMS; this library neither
executes nor cancels orders, redrives orphans, resumes trading, nor replaces
risk/compliance/mandate admission. Fee command compatibility does not activate
a Rust approval service or authenticate a human caller.

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

Fee commands retain exact serving-tenant attribution, COMMAND class, canonical
command schemas, delegated `user:` issuer, exact target and nonempty explicit
portfolio entitlement. The authenticated gateway/Consumer remains responsible
for binding that metadata; a string prefix is not independent authentication.
Only the frozen history and current complete posted journal supply changed
fees. Proposals retain sorted original/revised execution wire and a tenant-bound
digest, a 24-hour review interval and the existing 512-KiB review bound. Proposal
insert, unchanged-order CAS and proposed FACT enqueue share one transaction.
Approval insert, case CAS and approved FACT enqueue share another transaction.
A committed approval redelivery validates its original signing time, including
when the retry is after expiry. Same-person, changed-digest and expired new
approval commands preserve Go's distinct durable blocked reasons; observation
revalidation still propagates those refusals without synthesizing a block.
Recovery after approval reuses the existing complete journal/fee transaction;
no new financial formula or venue side effect is introduced.

Fee-command qualification requires the actual PostgreSQL/NATS differential
fixture; local compilation and source guards do not supply that evidence.
Go remains the rollback reader/writer, and any ownership handoff still requires
complete dependency proof, backup/recovery and explicit human authorization.

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
