# Archiver broker identity

Both `archiver` and `archiver-drain` read `SPIFFE_ENDPOINT_SOCKET`. Set it to
`unix:///run/spiffe/spire-agent.sock` with the workload socket mounted when
connecting to the workload-identity broker listeners. They pass the same rotating
mesh TLS configuration to Kafka and NATS; the drain still requires its NATS
single-writer gate before replaying events. Its command-line broker addresses do
not override the identity environment setting.

An unset socket retains plaintext local development. A configured source that
cannot initialize fails startup without falling back to plaintext. Broker
clients close before the identity source so certificate rotation remains
available throughout their lifetime.

The shared mesh authorization permits valid peer SVIDs in `kanz.internal`, not
only a Kafka-specific peer identity. Kafka construction is lazy and does not
prove a successful broker handshake. This wiring preserves the Kafka protocol;
it does not establish Redpanda URI-SAN principal mapping or ACL equivalence.
Production migration under #1413 still requires those independent proofs.


## Optional finite snapshot lifecycle

`ARCHIVER_LAKE_SNAPSHOTS=true` adds `lake.snapshot.>` to this existing tenant
archiver. Provision its retained `LAKE_SNAPSHOT` stream and the same-tenant
`lake.snapshot` Kafka topic first. The topic is a non-compacted FACT log with
30-day derived retention. The flag defaults off; putting this route in an
explicit `ARCHIVER_SUBJECTS` list without the flag refuses startup.

This route uses the existing `archiver` durable group and native work delivery
profile. It does not create another archiver. Malformed or foreign-tenant bytes
follow the existing Kafka `dlq.archiver` path; they require no new NATS domain
publish permission. A durable consumer record proves a registered delivery
contract, not that an archiver is currently alive or that Kafka accepted a fact.

The opt-in lake owner must qualify its own gateway, registry, stream, archive
consumer and process boundaries before activation. Its lifecycle landing is
conditional on that mode; the active source-first deployment is not changed by
these definitions. Default `nats-rebuild` does not restore this optional journal.
Registry, derived-file and journal disaster recovery remain unverified under
#1413/#60. No live rollout or backup acceptance follows from source checks.

### Disposable lake snapshot archiver fixture

The parent lake qualification compiles the actual archive package test binary,
records its SHA256, and invokes only `-test.run=^TestLakeSnapshotArchiverSubprocess$`
with `KANZ_LAKE_ARCHIVER_CHILD=1`. `TEST_LAKE_ARCHIVER_TEST_BIN` selects that
binary in the parent. A single newline JSON initialization (maximum 64 KiB)
carries loopback endpoints and ephemeral synthetic SVID/bundle bytes over stdin;
credentials are not environment variables or files. The helper uses `config.Load`
with `ARCHIVER_LAKE_SNAPSHOTS=true`, only `lake.snapshot.>`, and the existing
`archive.New`/`Run`/`Handle`, native NATS durable subscription and Kafka publisher.
It does not implement a second archival path.

`connected` means the native durable is registered and initially caught up, not
that a Kafka write has succeeded. The parent independently observes the consumer
and exact Kafka bytes, keys, tenant routing and DLQ/retry behavior. NATS requires
the exact synthetic archiver and broker SPIFFE identities; Kafka is restricted
to the disposable loopback fixture. This is not SPIRE issuance or Kafka ACL proof.
The native poison-message path remains Kafka `dlq.archiver`.

EOF cancels the 90-second helper context. `closed` is emitted only after Run,
both transports and the input reader have joined; lifetime expiry fails instead.
Native drain behavior is unchanged (five seconds plus two seconds stop slack).
The parent's separate cleanup observation allowance must accommodate that drain;
expiry requires failed cleanup and explicit process kill/join, never a successful
shutdown assertion. Logs are discarded and failure text is a fixed category.

`ExpectedAccount` is a fixture allowlist assertion only. It does not establish
the authenticated broker account; the parent must independently prove account
identity and same-account durable registration. Trailing input other than EOF
refuses after joined cleanup.
