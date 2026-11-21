# Disposable copilot identity proof

This Linux CI fixture creates its own kind/Cilium/SPIRE cluster through the
existing `tools/feed_identity` installation and setup helpers. Every kubectl
operation names its private kubeconfig and owned context. It never selects an
existing cluster, uses Windows-host NATS, calls an exchange or reads cloud
credentials. Failure artifacts contain verdicts and hashes, never SVIDs/keys.

Probe-labelled pods in **both** namespaces have ingress denied and egress only
to owned broker-labelled pods in `kanz-messaging`, TCP4222. They need no DNS or
external-network grant: addresses are private IPs and the Workload API is a Unix
socket. Observed policy objects must preserve those exact selectors/port. In
each namespace a probe must time out to an owned inert destination; a separate
control, permitted only to that destination, must read its fixed dummy response.
The inert process itself has no egress. This establishes Cilium enforcement and
a live-destination positive control without probing public or shared endpoints.
SPIRE/controller infrastructure remains separate from probe policy selection.

The broker mounts **all** canonical `kanz/infra/nats/tenancy.yaml` data unchanged,
including tenant routing and every identity grant. The mounted UTF-8 scalar is
checked against its source hash. The shared helper only removes single-node
cluster routes and substitutes disposable storage and already reviewed image
pins. No source grant, runtime pin, trust domain or SPIRE template changes.

The probe and the copilot's test binary are compiled on the selected host Go
toolchain, with generated canonical contracts and the warmed dependency cache.
A COPY-only, network-disabled build packages them in the existing pinned
distroless runtime. CI pre-pulls that base outside the credential-free proof.
This adds no Go builder, production command or released test endpoint.

The actual `buildDecisionRecorder` and `busAnswerRecorder` publish acknowledged
authorization allow/deny decisions and one answer. A different, genuinely
issued `nats-bootstrap` identity retrieves all three retained messages through
JetStream and checks their canonical envelope, payload, tenant and attribution.
The copilot is not granted stream administration or read access for verification.

Fourteen explicit broker publish refusals cover unprefixed and tenant-routed
submit/cancel/amend/approval, mandate/risk FACT forgery, the kill switch, and
stream/consumer administration. Four subscription refusals cover order, risk,
mandate and tenant-bridged data. These are broker authority checks; the companion
HTTP/gRPC tests separately prove route authorization. Canonical `_INBOX.>` is
allowed and is **not** claimed as a denied foreign-inbox boundary.

A genuinely issued but unregistered ServiceAccount must receive the broker's
authorization refusal. The production recorder also rejects a reachable broker
holding the genuine `nats-wrong` SVID; a separate client explicitly authorizes
that peer and connects successfully as a positive control. This drove the
recorder-only repair from mesh-membership authorization to the existing exact
canonical broker identity helper. Plaintext local development remains supported.

A second private broker mounts a **copy** with only the copilot's publish grant
widened to `>`, while retaining the valid canonical broker ServiceAccount. The
ordinary refusal probe must fail specifically at its first submit check, and
independent retained retrieval must find that forbidden publication. The source
and baseline broker ACL bytes remain unchanged. Arbitrary probe failure does
not count as detecting this mutation.

Run offline model tests with
`python3 -B -m unittest discover -s tools/agent_identity -p 'test_*.py'`.
The CI workflow invokes `python3 -B tools/agent_identity/proof.py` with
`AGENT_IDENTITY_EVIDENCE_DIR`, `RUNNER_TEMP`, `GITHUB_RUN_ID` and
`GITHUB_RUN_ATTEMPT`. It requires Linux protected filesystem/process APIs,
checksum-verified tools/chart, generated SDK, a warmed module cache and the
existing native Linux Docker engine. It emits `identity.json`, schema1, binding
the exact Git source/tree, CI run/attempt, canonical ACL hash, both binaries,
image identities, all32 individual cases and actual owned-resource cleanup.

All probes are bounded jobs. Cleanup removes only this unique cluster, its
containers, its fixture image and its private kubeconfig. Failures remain failed
even when cleanup succeeds. This is disposable developmental proof for #974;
production identity rotation, network posture and live authorization remain
separate operational acceptance.

Before any owned resource creation, an exclusive `ownership.json` records the
exact source/tree/run/attempt and the shared helper's generated
`feed-proof-<12 lowercase hex>` name, after confirming that target is absent.
An always-run CI step invokes `proof.py --cleanup-owned` with the same evidence
directory and run/attempt. It validates ownership against current Git, deletes
only that exact name and label-owned fixture image, and writes `cleanup.json`.
Missing ownership deletes nothing and makes no absence claim; stale, malformed
or foreign ownership refuses cleanup. It never scans prefixes for deletion.
The ownership receipt contains no kubeconfig path or credentials and is excluded
from artifact upload. The sanitized cleanup receipt is source/run bound.
