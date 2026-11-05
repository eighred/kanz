# Disposable Rust feed identity proof

This development fixture creates a new, uniquely named kind cluster on a Linux
CI runner, with the default CNI disabled and Cilium installed. It never uses an
existing kubeconfig, contacts Tokyo, runs public exchange protocols, or deploys
an authoritative service. No credentials or private keys are uploaded.

The test-only Rust harness calls the feeds' actual private `runtime::connect`,
canonical `Framer`, and durable-ACK `publisher::publish` paths. It is compiled
with `cargo test --lib`; it is absent from production release binaries. Each
feed uses its own ServiceAccount/SPIFFE identity, and the OKX case uses the OKX
framer. Canonical SPIRE CRDs/server/agent/CSI/registration and the NATS TLS/helper
configuration are reused. The only resource substitutions are disposable storage,
single-replica brokers, explicit image pins and fixture-only narrow NATS users.
The fixture does not certify production SVID rotation, CA recovery or live ACLs.

Required observations (no optional dependencies/skips):

1. TCP succeeds before policy; default deny blocks it; a namespace-and-pod allow
   restores only the selected client's access to port 4222.
2. Real SPIRE Workload API issuance succeeds before identity-negative tests.
   TLS maps the dedicated feed URI SANs to NATS users; both publish canonical
   book and quote FACTs with durable JetStream ACKs.
3. A valid but unregistered client identity is rejected. A broker with a valid
   `nats-wrong` SPIFFE identity is rejected by the Rust client's exact-peer policy.
   A test-only positive control explicitly authorizes that actual broker URI and
   must connect over mTLS. Unregistered-client refusal requires the typed broker
   AuthorizationViolation response and a new broker authentication-error diagnostic;
   a generic connection error is insufficient.
4. A feed cannot publish `fixture.forbidden`: require its failed request, the
   broker's explicit permission-denial diagnostic, and a final stream count of
   exactly four authorized FACTs. The forbidden subject is deliberately included
   in the stream so an accidentally allowed publication cannot disappear unnoticed.
5. Cluster deletion succeeds. Failure artifacts remain failed even if cleanup fails.

NATS fixture version 2.12.6 is digest-pinned and differs from the existing
`2.10-alpine` deployment reference. URI-SAN matching is implemented in its
[authentication source](https://github.com/nats-io/nats-server/blob/v2.12.6/server/auth.go),
not inferred from the tenancy manifest. No active NATS manifest is changed.

`install.sh` pins and checksum-verifies kind 0.33.0, Kubernetes/kubectl 1.36.4,
Helm 4.3.1 and Cilium's 1.20.4 chart. SPIRE's existing image digests are retained;
previously tag-only helper/registrar/busybox images receive explicit fixture pins.
Upstream sources: [kind release](https://github.com/kubernetes-sigs/kind/releases/tag/v0.33.0),
[Cilium on kind](https://docs.cilium.io/en/stable/installation/kind/),
[SPIRE Kubernetes](https://spiffe.io/docs/latest/try/getting-started-k8s/).

Run offline tests with `python3 -B -m unittest discover -s tools/feed-identity -p 'test_*.py'`.
The `feed-identity-proof` workflow installs the pinned tools, builds the test-only
container and invokes `proof.py`. It has read-only contents/packages permissions.
The sanitized artifact records source SHA, image identities, test verdicts and
cleanup. A passed fixture is developmental identity/enforcement evidence under
#1413; it is not production promotion or closure of all #232 namespace gaps.
