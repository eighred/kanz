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

### Retained Go admission at the writer fence

Follow the [synthetic order build instructions](../../../kanz/dev/synthetic-orders.md) for Python 3.11+, Docker Linux amd64, the module's Go toolchain, generated Go SDK and Rust 1.90 native binary. Cache the digest-pinned PostgreSQL and Rust runtime images used by the synthetic order drill. From the repository root run:

```sh
python3 -B kanz/dev/synthetic_writer_lineage.py --native rust/target/debug/kanz-synthetic-oms
```

The dedicated database first permits a generation change in a rolled-back rehearsal. The Go store then admits one unfunded order and queues its acceptance. Two attempted transfers must refuse the retained order's missing command lineage, preserve the original state and pending fact, and fence a successor submission. This fixture leaves the unmatched order intact until its disposable database shuts down.

For a forward transfer with verified source provenance, add `--forward`. This uses a separate funded/disabled-posture fixture and builds the tagged, loopback-only `syntheticadmission` Go executable. A trader authenticated by the gateway submits one direct unfunded XSIM spot buy. The independent control writer binds the exact broker frame to its entitlement and records risk, compliance, mandate and market evidence. The Go order store commits the original frame, command, five source IDs, canonical order and pending acceptance together. Generation two reads the same bytes, confirms a duplicate submit and records one dispatch claim from the recovered JetStream acceptance. A second claim requires reconciliation. The drill stops before any venue send. Armed or missing funding posture, unsupported orders, expired decisions and legacy evidence without a frame binding refuse admission.

For the same order across three writer generations, run:

```sh
python3 -B kanz/dev/synthetic_writer_lineage.py --native rust/target/debug/kanz-synthetic-oms --roundtrip
```

Go generation one commits the signed admission before acknowledging the delivery. Rust generation two recovers the original bytes, publishes the retained acceptance and sends one order to the synthetic venue. The database refuses a Go handback while the dispatch result is unknown. After a typed venue query establishes a working order, Go generation three drains the routed fact and runs the ordinary startup sweep. The same order version and both outbox rows survive. The synthetic venue reports one placement, two queries and zero fills.

### Funded terminal order

```sh
python3 -B kanz/dev/synthetic_writer_lineage.py --native rust/target/debug/kanz-synthetic-oms --funded-roundtrip
```

This profile uses the same signed Go admission and retains the original funded reservation with the order. Rust applies one partial execution and confirms withdrawal of the remainder through XSIM. The database refuses handback while withdrawal is unresolved. The funded recovery role can activate Go after terminal confirmation when the signed withdrawal, settlement authorization, execution journal, queued facts and unbooked cash obligations agree.

Go starts its ordinary outbox relay, startup sweep and JetStream command consumer. The tagged XSIM successor verifies the original gateway withdrawal against the published receipt inside the ordinary OMS cancellation handler. A real redelivery and a fresh consumer both acknowledge that command without another XSIM call or publication. A withdrawal without an exact matching receipt fails closed in this profile. Accounting books 126 USD once, leaving 874 USD and releasing all remaining holds. Both retired Rust logins are fenced. This profile covers the synthetic terminal state with one partial execution before accounting has booked it. Deployment and other funded recovery states require their own acceptance evidence.

### Funded order still working after a partial fill

```sh
python3 -B kanz/dev/synthetic_writer_lineage.py --native rust/target/debug/kanz-synthetic-oms --funded-working
```

The first send loses its acknowledgement and the transfer is refused while the remote result is unknown. Rust recovers one 0.625-unit fill, leaving 0.625 units working. The dedicated Go recovery role requires an acknowledged working order with the outgoing writer's exact fill and no retained withdrawal claim. It checks the queued routed and partial-fill facts and preserves the 252 USD and 1 BNB holds at transfer.

Go drains the two retained facts and runs its startup sweep against the verified XSIM account. The recovery profile validates the cumulative quantity and complete execution against the journal before adoption can skip the existing fill. It blocks venue mutations and does not start trading subscriptions. Accounting then books 126 USD once, leaving 874 USD total and holds of 126 USD and 1 BNB while the order remains working.

### New withdrawal after Go takes ownership

```sh
python3 -B kanz/dev/synthetic_writer_lineage.py --native rust/target/debug/kanz-synthetic-oms --funded-withdrawal
```

After startup a gateway-authorized cancellation runs through the normal OMS JetStream consumer. The dedicated Go login claims the exact authorized command in PostgreSQL before the typed XSIM cancellation. The bridge exposes only claim and finish, reusing the settlement guard and cash transaction. A lost response leaves the original state and both holds intact. Redelivery can query the same venue order but cannot send another cancellation. Only complete history matching the retained execution and an independently authorized terminal observation can release the unfilled remainder and enqueue the cancellation fact.

This isolated profile covers one previously observed partial fill and one subsequent cancellation, including lost acknowledgement and command replay. A confirmed terminal write keeps 126 USD reserved until Accounting books it. After posting, the balance is 874 USD with no remaining reservation. Additional fills, expired attestations, deployment approval, and production capacity still need their respective acceptance evidence.

### Restart across withdrawal and outbox commit

```sh
python3 -B kanz/dev/synthetic_writer_lineage.py --native rust/target/debug/kanz-synthetic-oms --funded-withdrawal-restart
```

Three separate Go processes use the same writer login and JetStream durable. The first exits after XSIM has withdrawn the order and lost the response, before the handler can ACK. Startup defers the proven current-writer withdrawal claim to its retained command while the ordinary sweep remains active. The second process queries XSIM on redelivery and exits after PostgreSQL commits the terminal state and before the outbox publisher sends it. The third startup drains that one fact and acknowledges the third delivery without contacting XSIM again. The runner compares the cash holds, retained state, broker acknowledgements and published payload across both process cuts.

For an exit after JetStream accepts the terminal fact and before PostgreSQL marks it published, run:

```sh
python3 -B kanz/dev/synthetic_writer_lineage.py --native rust/target/debug/kanz-synthetic-oms --funded-withdrawal-publish-mark
```

Go and Rust relay attempts use the row's persisted UUIDv7 as the JetStream message ID. The new process retries the same terminal row, marks it published, and acknowledges delivery three. The proof requires the original JetStream sequence, UUID and frame bytes to survive the retry and verifies one Accounting debit. This broker suppression applies within the stream's duplicate window. Consumer idempotency still applies after that window. Before upgrading an existing publisher estate, reconcile pending rows that an older publisher might already have sent under a per-attempt UUID. Such an identity cannot be reconstructed from the old SQL row.

### Concurrent delivery after funded handback

```sh
python3 -B kanz/dev/synthetic_writer_lineage.py --native rust/target/debug/kanz-synthetic-oms --funded-withdrawal-race
```

Two Go processes consume the same signed cancellation on separate JetStream durables. The runner holds the winning XSIM cancellation until the other process observes the original working fill. At that point PostgreSQL has one claim and both reservations are still intact. It drops the cancellation acknowledgement and waits for both retries to query XSIM before returning the confirmed terminal view. Both processes attempt settlement. The test requires one `APPLIED`, one `DUPLICATE`, both acknowledgements, one terminal fact and one Accounting debit. The separate working-query probe rejects retained fill IDs with changed fees and incomplete histories through the same history check used before settlement.

### Contradictory terminal reply after handback

```sh
python3 -B kanz/dev/synthetic_writer_lineage.py --native rust/target/debug/kanz-synthetic-oms --funded-withdrawal-contradiction
```

After the lost cancellation acknowledgement, XSIM returns the recorded fill ID with a changed fee over mTLS. Go refuses that reply. Before the next query returns the original receipt, the profile checks the retained partial order and version, pending withdrawal, three outbox records and cash holds. Delivery three settles the cancellation. A fresh durable replays it, and Accounting books the original 126 USD once.

### New execution after Go takes ownership

```sh
python3 -B kanz/dev/synthetic_writer_lineage.py --native rust/target/debug/kanz-synthetic-oms --funded-remainder
```

The order remains working with one recorded 0.625 SYN execution when Go
takes ownership. The disposable XSIM venue records the other 0.625 SYN
after the handback. Its ordinary typed query returns both executions and the
cumulative 1.25 SYN. Each execution occurs at 200 USD with a 1 USD fee.

The verifier binds that history to the retained G2 execution and authorizes
the exact G3 result. The scoped PostgreSQL function claims the new execution,
advances the order, records the cash obligation and queues one filled fact in
one transaction. A repeat query uses the same execution and fact. After two
separate 126 USD Accounting inclusions, the balance is 748 USD and 8 BNB, with
zero held in both currencies. This profile verifies the symmetric two-fill
fixture. Production deployment and other settlement shapes retain separate
acceptance.

Add `--funded-remainder-first-booked` in place of `--funded-remainder` to
book the first execution after handback and before the new venue observation.
Go's first startup drains the retained facts and verifies the working partial
order. Accounting leaves 874 USD total and 126 USD held. A second Go process
under the same writer generation reconciles the new execution, releases the
BNB hold and preserves the booked debit until Accounting includes the second
execution. The resulting balance and both cleared holds match the first profile.
