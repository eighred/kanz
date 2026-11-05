# Disposable Redpanda identity qualification

This #1413 fixture tests the Kafka protocol identity boundary using SPIRE 1.15.3,
Redpanda 26.2.6, and the existing pinned kind/Cilium fixture tools. It never
applies configuration to an existing cluster. The existing NATS identity proof
remains separate and unchanged.

SPIRE's built-in `uniqueid` CredentialComposer adds subject OID `2.5.4.45` from
the first 128 bits of SHA256 of the complete SPIFFE ID. The URI SAN remains the
canonical identity. Four exact DN rules map only the fixture identities to
distinct aliases; there is no DEFAULT or wildcard rule. A dedicated bootstrap
identity creates two topics and grants each client only read/write/describe on
its own topic. The fourth identity has no ACLs. Only bootstrap is a superuser;
no tested client receives its credentials or ServiceAccount.

The actual segmentio Kafka client requires the exact broker SPIFFE ID. Both
clients must publish with all acknowledgements, read their own bytes, receive
explicit Kafka authorization errors for cross-topic reads/writes and ACL
administration, observe a different real SVID serial within 150 seconds, discard
pooled TLS connections, construct a new transport/client and repeat
own-topic publication and cross-topic write refusal after renewal. Registration
TTL is 60 seconds only in this fixture. An ungranted SVID must receive explicit
topic authorization errors. The `proof-ungranted` identity has no grant in
the topic ACL inventory; it has a valid SPIRE registration and a distinct DN
mapping. This case does not prove refusal of an unmapped certificate subject.
Final bootstrap reads require exactly two expected
records per topic, so rejected writes cannot silently have succeeded.

`redpanda-identity-proof.yml` runs the mandatory fixture on Linux with no optional
skip. Local unit checks:

```sh
python3 -B -m unittest discover -s tools/redpanda-identity -p 'test_*.py'
cd kanz
go test -p 1 ./tools/redpandaidentity -count=1
go vet ./tools/redpandaidentity
```

The Python boundary mutations are bounded source/config assertions, not semantic
proof of arbitrary Go. Real broker assertions supply the integration evidence.
Artifacts retain only source/image identities, public fixture principal/DN
strings, verdicts and cleanup status. Private keys stay in a memory-backed
fixture volume and are never logged or collected. Failure or cleanup failure
cannot produce a passing artifact. The admin API binds only loopback; Kafka has
only the authenticated TLS listener. Single-node internal RPC binds loopback.

This is not a production mapping rollout, financial event test, durability or
CA disaster-recovery proof. It does not establish deployed Kafka plugin parity,
retained offset continuity, arbitrary tenant provisioning or current Go
`AuthorizeMesh` broker-peer narrowing. A production migration still needs the
reviewed mapping/ACL inventory and coordinated issuance, rollout and rollback.
No live data, active manifests or production permissions change.

Primary references: [SPIRE uniqueid](https://github.com/spiffe/spire/blob/v1.15.3/doc/plugin_server_credentialcomposer_uniqueid.md),
[hash implementation](https://github.com/spiffe/spire/blob/v1.15.3/pkg/common/x509svid/uniqueid.go),
[Redpanda DN mapping](https://docs.redpanda.com/streaming/current/manage/security/authentication/),
[first-start bootstrap](https://docs.redpanda.com/streaming/25.2/deploy/redpanda/manual/production/production-deployment/).
The CEL composer currently implements JWT composition only and is not used.
