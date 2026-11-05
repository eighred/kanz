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
