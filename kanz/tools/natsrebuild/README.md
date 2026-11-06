# Archive transport during recovery

`nats-rebuild` reads bounded Kafka ranges and republishes onto the existing NATS
subjects. It retains tenant topic derivation, malformed-frame handling and
consumer-group-free reads. It is not an authoritative TV opening-history importer:
complete history, account-level ordering and knowledge-time provenance require
separate acceptance under #1410/#1413.

The existing Job still selects the plaintext Kafka listener. For a protected
Kafka/Redpanda listener, promote the verified image, broker address and
`NATS_REBUILD_KAFKA_MTLS=true` together. This requires
`NATS_REBUILD_SPIFFE_SOCKET`; startup refuses an unavailable configured identity
or an invalid flag. The same rotating workload source remains open for NATS
publication, Kafka metadata, offset discovery and bounded partition reads.
Unset/`false` preserves the old Kafka configuration, including deployments that
already use mTLS only for NATS. This source change does not alter a Job or listener.

The generic reader also carries explicitly supplied SASL through each connection;
the CLI does not introduce SASL credential environment variables. The mesh client
still uses the existing trust-domain peer policy. Exact Redpanda principal mapping,
topic ACLs, workload registration and peer-policy narrowing need coordinated target
acceptance; TLS configuration alone is not that proof.

Closing a reader before its first read, after failed discovery, or during stalled
metadata now terminates its owned work. Metadata RPCs have deadlines as well as
dial timeouts. No authentication failure retries through plaintext. The real
Redpanda identity fixture requires the same bounded reader before and after SVID
renewal, exact captured offsets/EOF, and observed wrong-peer authorization refusal
followed by cleanup. Existing source records and consumer offsets remain unchanged.

Run local logic checks with `go test -p 1 ./tools/replay ./tools/natsrebuild/...`.
Real broker and Linux race evidence comes from the mandatory workflow and cannot
be inferred from local dependency skips. Restore/publish authority and deployment
remain governed by the existing recovery procedure.
