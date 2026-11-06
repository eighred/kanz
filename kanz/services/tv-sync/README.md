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
checkpoint and a 120-second deadline. Unsupported/unknown protobuf fields,
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

CI requires five real-PostgreSQL verifier cases, including restored bytes,
tenant isolation, read-only enforcement and a concurrent append outside the
captured snapshot and refusal of foreign rows under a permissive RLS policy.
Two required Linux secret-reader cases cover bounds and FIFO/CSI behavior.
Those fixtures qualify the source behavior; they do not
substitute for observing replay of the actual restored target with its matching
verified image. No activation manifest is changed by this prerequisite.
