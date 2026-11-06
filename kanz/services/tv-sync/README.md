# tv-sync listener migration

This source release supports an opt-in split while keeping existing deployments
compatible. With `TV_SYNC_API_LISTEN` absent, `TV_SYNC_LISTEN` (default `:8091`)
continues to serve the Broker API, health, readiness and metrics together. This
legacy mode retains the monitoring-to-tenant-API exposure tracked in #232; it is
not an isolation claim. The deployed manifests and the named guard exemption
remain unchanged until image promotion.

Set `TV_SYNC_API_LISTEN=:8113` to select the split. The Broker API then exists
only on that listener, while `TV_SYNC_LISTEN` serves only `/healthz`, `/readyz`
and `/metrics`. Explicit empty or malformed API configuration refuses startup;
a failed bind never falls back to the legacy listener. All selected sockets
must bind before readiness and all drain on shutdown.

Broker handlers trust the gateway's `X-Kanz-Principal-*` headers. NetworkPolicy
must make the gateway the API listener's only reachable caller; neither the
headers nor the new listener independently authenticate a gateway workload.

## Release and promotion order

1. Merge and verify the source with the existing deployment manifests intact.
   Both the old binary and this release remain compatible with those manifests.
2. Publish the reviewed merged revision through the approved signed-image path.
   Verify its immutable digest, signer identity, SBOM and vulnerability evidence.
   A passing PR build or unsigned main image is not promotion evidence.
3. On a controlled enforcing-CNI test cluster, validate the matching image with
   API 8113 and operations 8091, the two Service ports, gateway URL, and base and
   environment-specific NetworkPolicies. Keep the singleton `Recreate` strategy.
4. Promote that verified digest and the listener configuration together in a
   reviewed manifest change. Move tv-sync's guard entry from the shared-listener
   map to the split-listener map in that same change. Do not change the gateway
   port while retaining an image that lacks the split.

Before promotion, prove authorized gateway reads succeed on 8113, monitoring
can scrape and probe 8091, Broker routes return 404 on 8091 even with forged
principal headers, and monitoring/unrelated pods cannot connect to 8113 by
Service or pod IP. Check missing-identity refusal, tenant scoping, restart and
fact-log recovery without introducing a second consumer replica.

Retain the matching prior image and manifests for coordinated rollback. Removing
`TV_SYNC_API_LISTEN` restores legacy shared routing, including its exposure;
rolling only the image or only the gateway port can cause an outage. No release
or rollback changes the capital halt.

Loopback HTTP and static policy tests establish source behavior. They do not
prove an image was published or a CNI enforced policy. #232 stays open until
its operational acceptance is observed on the promoted revision.

## Read-only restored projection qualification

The image also contains `/tv-sync-replay`. This operator command compares a
checkpoint plus its tail with a full replay through the same production
`Projection.Rehydrate` fold. It opens one tenant-scoped PostgreSQL transaction
with `REPEATABLE READ READ ONLY`, requires a NOSUPERUSER/NOBYPASSRLS role and
FORCE RLS on both tables, and rolls back even on success. It does not subscribe
to NATS, serve requests, save checkpoints, append facts, or publish financial
events. Marks are absent: this qualifies stored order/fill state and cost basis,
not current market valuation.

Run only a reviewed, signed image containing this command against the intended
restored database. Supply `TV_SYNC_TENANT`, explicit `TV_SYNC_RETENTION` (24h to
8760h), `TV_SYNC_REPLAY_EXPECTED_VERSION` matching the image's link-time stamp,
and `TV_SYNC_DATABASE_URL_FILE` pointing to the runtime-role DSN mount. Plaintext
`TV_SYNC_DATABASE_URL`, command-line arguments and unstamped binaries refuse.
Mount secrets through the existing operator/workload mechanism; do not paste
DSNs into commands or evidence. The old `v2026.4.0` image predates this command;
copying that artifact does not qualify restored-state replay.

The bounded mounted-secret reader requires Linux, opens with `O_NONBLOCK` and
checks the opened descriptor is a regular file. CSI symlinks to regular files
are intentionally supported; FIFOs and symlinks to FIFOs refuse. Other platforms
refuse this new mounted-file mode; legacy secret readers are unchanged. This
prevents the tested special-file blocking case, not arbitrary hung-filesystem
I/O. The verification transaction has its own deadline; the byte ceiling is not
a claim of a hard filesystem timeout.

Qualification is bounded to 100,000 facts, 64 MiB of aggregate fact payload and
identity/type bytes, 64 KiB per payload, 1 KiB per identity/type, an 8 MiB
checkpoint and a 120-second deadline. Exact in-memory execution, baseline and
live-position rationals are compared before checkpoint serialization, so decimal
rounding cannot hide a difference; their bounded canonical integer encodings are
included in the state digest. Unsupported/unknown protobuf fields,
invalid timestamps, out-of-domain decimals and unsupported checkpoint rationals
refuse. Exceeding a bound does not return a prefix result. Both reconstructions
use one declared retention horizon, preserving exact baseline rationals; a
checkpoint that has already discarded the requested horizon refuses.

Output contains fixed verdicts, source version, tenant/snapshot/input/state
hashes, checkpoint sequence, watermark, count and as-of time. It contains no
facts, account identifiers, credentials or raw database errors. A successful
result proves equality only for that SQL snapshot. Bind it independently to the
reviewed image, restored archive and target database; it is neither a durable
health lease nor protection against later privileged changes. A completed
migration Job or `RESTORE_VERIFIED` receipt alone is not this proof.

An empty history or absent checkpoint is **not qualified restored state**. A
fresh Tokyo database therefore needs a separately reviewed genesis/history
bootstrap prerequisite. Current service startup marks readiness after launching
consumers; it does not wait for a historical-backlog watermark. The canonical
EXECUTION stream retains 24 hours, and durable consumer replay does not prove
coverage before retention. Establish the required source/history coverage and
bootstrap readiness before activation; do not fabricate facts or use an empty
database as evidence that an account never traded. This remains tracked under
[#1410](https://github.com/eighred/kanz/issues/1410) and
[#1413](https://github.com/eighred/kanz/issues/1413).

CI requires ten real-PostgreSQL checkpoint/verifier cases, including an exact
checkpoint restart with subsequent fills/cancellation and P&L, restored bytes,
tenant isolation, read-only enforcement and a concurrent append outside the
captured snapshot and refusal of foreign rows under a permissive RLS policy.
The precision regression refuses a resident 1e-10 execution rounded to zero by
the legacy checkpoint encoder. The exact encoder below prevents new lossy
checkpoints; the verifier cannot repair previously rounded data.
Two required Linux secret-reader cases cover bounds and FIFO/CSI behavior.
Those fixtures qualify the source behavior; they do not
substitute for observing replay of the actual restored target with its matching
verified image. No activation manifest is changed by this prerequisite.

### Exact checkpoint encoding and controlled upgrade

New checkpoints use `tvsync.v1.Checkpoint.exact_encoding_version = 1`.
Retained execution quantities, prices and fees are encoded through
`dec.ToProtoExact`: every digit survives or checkpoint creation fails before
writing. Nil remains missing; zero remains present. The existing exact converter
accepts exponents within ±60 and an int64 coefficient, so some valid incoming
Decimals at ±61..64 cannot be checkpointed. They are not rounded or replaced;
checkpoint failure leaves the prior durable checkpoint unchanged. Baseline
rational strings, order revisions and their ordering are unchanged.

Version 0 checkpoints may already have lost precision through the former
scale-eight conversion. Version 0 and unknown versions now refuse startup before
restoring state. Inspecting their decimal values cannot establish which digits
were lost. Do not relabel, automatically delete, or silently replay past them.
Recovery requires independently establishing complete raw history, rebuilding the
projection from that history under a controlled procedure, and verifying the
result before producing an exact checkpoint. Replay of only currently visible
facts does not establish archived-history completeness or rule out a missing
prefix. This source change does not perform that operational recovery.

Use a controlled writer handoff. Old readers ignore the additive marker and old
writers can overwrite an exact checkpoint with a legacy checkpoint during a
mixed-version rollout. Stop old writers before starting the new writer or
restarting against its checkpoint; deploy matching schema, verifier and service
versions. Protobuf wire compatibility alone does not make a mixed writer rollout
safe. Existing live images and deployment configuration are unchanged here.

### Startup readiness and retained history

Broker endpoints return 503 until every configured OMS FACT and mark subscription
has reached initial durable catch-up. The existing durable identity, queue group,
delivery, deduplication, retry/DLQ and shutdown-drain behavior remain in use.
Readiness waits for both pending and unacknowledged broker deliveries; merely
launching a consumer or receiving its last message cannot open the API. Process
cancellation closes readiness even if a catch-up callback races with shutdown.

This is an initial transport barrier, not historical completeness or continuous
broker-health attestation. Existing DLQ/ignored-monitor-fact behavior is unchanged.
A drained retained stream cannot prove inception history: EXECUTION retains 24h,
and an empty local fact log plus an empty stream is not a qualified empty fund.
Fresh-genesis/history provenance and the serving tenant mapping remain separate
activation prerequisites under #1410/#1413; this change grants no empty-book
permission and does not replace the isolated restored-state verifier.

### Append and checkpoint ordering

Concurrent subject handlers serialize durable append, in-memory fold and delta
publication in one process. The database wait uses a separate mutex so readers
and checkpoints can continue to observe the last fully folded prefix. A later
append cannot overtake an earlier committed insert whose response is delayed;
otherwise a checkpoint could skip that earlier fact on restart. Mandatory local
and real-PostgreSQL tests hold that response while taking a checkpoint, then
verify the recovered position and final watermark. This is not a distributed
writer lease, complete-history proof or permission to run multiple writers for
the same tenant. The mutex alone cannot resolve an ambiguous append outcome;
the durable-prefix recovery below supplies that separate boundary.

Durable append uncertainty (#1410/#1413): live appends take a tenant transaction
advisory lock in a separate statement before sequence allocation, and release it
only when the transaction resolves. `BIGSERIAL` alone is not commit ordering.
Every successful append, including duplicate delivery, replays the stored suffix
after the local folded watermark. Stored bytes and knowledge times are retained;
a partial replay error returns a retry while retaining only its completed prefix.
An append error may mean committed: it does not fold or claim rollback. The next
FACT delivery resolves the append boundary and repairs that prefix without polling.

This is not a multi-writer lease or a repair for an already-invalid checkpoint.
Stop/drain old append writers before starting this version: they do not take the
new lock. Unresolved old transactions must finish first. Existing checkpoint
completeness still requires the restored-history verifier and independently
established history coverage; do not delete, relabel or trust a checkpoint merely
because this fix is installed. No production handoff is performed by these tests.
The PostgreSQL regression wraps a genuinely committed append with a lost-response
error, and separately tests two database connections, lock timeout, rollback gaps
and tenant isolation. It does not simulate a real network partition or prove a
multi-replica deployment safe. All new cases are mandatory in replay CI evidence.

Recovery here means recovery on a subsequent successful delivery, **not guaranteed
automatic redelivery**. The current TV consumer uses `WithDLQ` with the default
single attempt: a transient append/replay error can be parked and deduplicated.
With no later FACT, the missing fold may remain, and transport readiness alone
does not establish projection completeness. The retained-retry profile currently
refuses finite-delivery `order.*`; this fix does not change that global profile.
TV-specific retry/serving failure behavior and actual NATS plus PostgreSQL proof
remain activation prerequisites tracked in #1410/#1413.

### Retained FACT delivery and serving refusal

TV now uses a dedicated order-FACT NATS client with the existing work timing and
concurrency budget and unbounded delivery attempts. Its Consumer validates the
actual effective profile used to create the durable, and retains transient
failures without DLQ parking or a completed dedup claim. Price subscriptions
keep their original finite profile; the global order profile and existing audit
retained-delivery caller are unchanged. Terminal malformed bytes retain the
existing DLQ policy. This does not extend stream retention or prove history that
predates the retained source.

The Broker API and readiness require initial durable catch-up, a live process,
and no unresolved fold failure. Handler execution and readiness outcome update
are serialized together. A success clears only the same tenant/event failure;
unrelated delivery or a late catch-up callback cannot reopen serving. The map
holds only hashed identities, is capped at 4096, and missing identity or overflow
stays closed for that process. Panic also revokes serving before bus-owned panic
handling. A connection loss stops the process instead of treating reconnect as
freshness proof. Restart must rebuild and pass the durable catch-up barrier.
Operations health remains separate; readiness and Broker responses expose only
a bounded not-ready status, never event or tenant identifiers.

Price filters require a literal first token disjoint from `order`; wildcard or
empty roots and `order` roots refuse startup. This preserves the default market
filters and literal custom price roots while avoiding a second NATS wildcard
parser. Controlled single-writer handoff, complete-history qualification, and
recovery of already-invalid checkpoints remain separate prerequisites. No live
activation or new multi-replica ownership is established here.

`TestTVRetainedFactsRecoverWithNATSAndPostgres` requires actual NATS plus a
NOSUPERUSER/NOBYPASSRLS PostgreSQL role. It covers a committed append with an
injected lost response, actual database lock timeout, unacknowledged durable
restart, no-later-FACT redelivery, Broker 503/200, unchanged single durable fact,
and a real terminal DLQ positive control. It does not inject a network partition
into PostgreSQL. Required CI rejects skipped dependencies and requires every
scenario; local source/unit checks alone are not operational acceptance.

Existing finite TV durables keep the same group/name/filter. The existing bus
`inspectExistingDurable` + `CreateOrUpdateConsumer` path already logs supported
tuning drift and updates timing/delivery settings in place; it refuses a changed
filter. The new code reuses that mechanism, never deletes/recreates a durable or
falls back to 64. `TestRetainedProfilePreservesExistingDurableCursor` requires an
actual 64-to-unbounded update with identical name/creation time and preserved ack
floor, delivering only the pending suffix. Old consumers must be stopped first:
an old process could reassert 64 on the same durable. This is a controlled source
handoff requirement, not proof that any deployed estate has performed it.
