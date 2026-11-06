# Disposable Redpanda identity qualification

This #1413 fixture tests the Kafka protocol identity boundary using SPIRE 1.15.3,
Redpanda 26.2.6, and the existing pinned kind/Cilium fixture tools. It never
applies configuration to an existing cluster. The existing NATS identity proof
remains separate and unchanged.

SPIRE's built-in `uniqueid` CredentialComposer adds subject OID `2.5.4.45` from
the first 128 bits of SHA256 of the complete SPIFFE ID. The URI SAN remains the
canonical identity. SPIRE 1.15.3 also issues `C=US,O=SPIRE` and a CN equal to
the first DNS SAN from the existing registration. The probe requires exactly
those four attributes with no duplicates/extras, exact URI/hash binding, and
a bounded DNS-shaped CN matching the first DNS SAN. Four anchored DN rules pin
the complete uniqueid and fixed C/O, allowing only the bounded CN field to vary.
The artifact records Go `pkix.Name.String()` order. The pinned Redpanda image
actually reports `C=US,O=SPIRE,CN=<dns>,x500UniqueIdentifier=<32hex>` (run
42120249766); broker rules match only that observed form. This supersedes the
initial inference from generic GnuTLS source, which incorrectly predicted a
numeric OID and DER hex. Go string equality alone cannot qualify broker mapping.
The CN never selects a principal; there is no DEFAULT or catch-all rule.
A dedicated bootstrap
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
The first Linux qualification (42113920778) reached the broker but correctly
failed bootstrap SVID validation because the initial probe incorrectly assumed
a sole-uniqueid subject. The corrected shape follows the pinned SPIRE
[credential builder](https://github.com/spiffe/spire/blob/v1.15.3/pkg/server/credtemplate/builder.go).
Operational acceptance still requires the corrected real-broker run to pass.

Run 42116497605 passed that SVID validation but failed broker bootstrap, with
cleanup confirmed. A separate exact-peer TLS handshake and fixed diagnostic
classes now distinguish TLS, topic creation and ACL creation; raw error text
is never emitted. This remains failed qualification until a complete run passes.

Run 42118276581 established exact-peer TLS success followed by explicit Kafka
topic authorization failure (code 29). Run 42120249766 retained the actual public
broker DN above, with no invalid bootstrap properties or missing bootstrap file.
The subsequent strict mapping correction still requires all five real cases,
including renewed TLS connections and final exact records, to pass.

Run 42121721369 passed bootstrap and client-a own-topic publish/read, then failed
the negative path. The pinned kafka-go Transport routes operations through
filtered metadata; its [Produce broker selector](https://github.com/segmentio/kafka-go/blob/v0.4.51/protocol/produce/produce.go)
returns a local missing-topic error before sending a request for a hidden topic.
That cannot prove broker denial. Negative operations now use `kafka.Conn` to the
same fixed fixture broker and partition over the same exact-peer SPIFFE TLS,
with an own-topic readback positive control through that path. Only the actual
broker's topic-authorization code 29 qualifies; local errors, timeouts, unknown
topics and accepted operations still fail. Normal positive and renewed writes
retain the pooled transport, and final reads still require exactly four records.


The disposable Linux runner preflight requires 24 GiB free on the verified Docker
filesystem before building the fixture. If needed it removes only the verified,
unrelated preinstalled Android SDK at `/usr/local/lib/android/sdk` on a
GitHub-hosted Ubuntu runner. It refuses aliases, nested mounts, unknown installation
identity, incomplete removal, and insufficient readback. It does not remove caches,
repositories or container volumes. This preparation is not permitted on local or
self-hosted machines.

Before every broker probe, qualification reads the actual `storage_min_free_bytes`
through the pod-local admin endpoint and the broker data filesystem capacity. It
requires available bytes strictly greater than that unchanged reserve plus 2 GiB
of fixture headroom. Only numeric observations are retained; raw cluster config
is never exported. The gate cannot establish that free space stays constant after
its observation, so any later broker refusal still fails the proof.

Run 42275410382 failed the first client-a Produce with Kafka storage code 56;
bootstrap had passed and cleanup succeeded. Its observed 5,356,982,272 available
bytes was below the documented default reserve of 5,368,709,120 bytes, although
log classifiers showed no space alert. That supports a reserve-pressure diagnosis
but does not prove the old runtime setting, which was not recorded. The new source
requires actual setting and capacity evidence rather than reducing broker safety.
See [the storage reserve contract](https://docs.redpanda.com/current/reference/properties/cluster-properties/#storage_min_free_bytes)
and [the hosted Android SDK inventory](https://github.com/actions/runner-images/blob/main/images/ubuntu/Ubuntu2604-Readme.md).
