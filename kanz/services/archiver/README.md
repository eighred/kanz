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
