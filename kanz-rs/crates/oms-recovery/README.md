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

An individual emitted FACT remains bounded to 1 MiB. A larger individual record
is an explicit unsupported Rust domain and refuses before the financial commit;
Go may instead enqueue it and later encounter broker limits. It is not universal
Go wire/operational parity. Broker maximum payload and stream size are read back
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
30-second maximum; it does not batch ACKs.

The separate `recovery-coordinator` job in the existing `rust-oms-fold` workflow
retains every original seven-package OMS proof. Its independent Go oracle runs
actual PostgreSQL, NATS and synthetic rotating SPIFFE mTLS. Large Go and Rust
8,000-fill cases run separately and exercise all 16,000 book ACKs. The dedicated
primary PostgreSQL fixture explicitly provisions `max_locks_per_transaction=1024`,
`max_connections=100`, `max_prepared_transactions=0`; a second unchanged-default
64/100/0 instance proves lock-capacity failure without a financial prefix. Both
use NOSUPERUSER/NOBYPASSRLS and twelve FORCE-RLS owner tables. This is disposable
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
