# Native tenant topic and ACL provisioning

`kanz-kafka-provision` is an opt-in Go administrative tool used by the existing
`infra/tenancy/tenantctl.sh onboard` caller. The source default remains
`TENANTCTL_KAFKA_MODE=legacy`. Native offboard is unsupported and refuses before
onboarding/offboarding effects; it never silently falls back to JVM tooling.
This tool does not replace the broker, PVCs, stored history, single archiver, or
existing approval process. Source review and local tests do not establish actual
Linux qualification or authorize a live handoff.

## Resource and identity inputs

Before selecting native mode, an authorized operator must obtain the intended
estate's ClusterID through a **separate read-only Kafka Metadata observation**
using approved tooling, authenticated workload credentials, and the exact broker
SPIFFE peer. Compare that observation with the intended estate's controlled
inventory and record the selection through the existing operational process.
Missing, empty, unknown, or inconsistent IDs refuse provisioning. Do not infer an
ID from DNS, reuse an ID solely because another estate has the same SVID, or
pipe a discovery result directly into apply. This tool has no discovery or
automatic identity-adoption mode; even `plan` requires the selected expected ID.

The supported ID subset is ASCII letters, digits, `_` and `-`, optionally
preceded by the exact `redpanda.` prefix, with a total limit of 128 bytes.
`redpanda.initializing` and `redpanda.unknown` refuse. Redpanda allows broader
configured strings upstream; those are unsupported here, not normalized into an
accepted ID. Every broker observation must still equal the caller's exact pin.

Native `tenantctl` requires the existing onboarding prerequisites plus:

| Input | Required value |
| --- | --- |
| `TENANTCTL_KAFKA_MODE` | `native`, explicitly selected |
| `TENANT` | One valid non-system tenant label |
| `NATIVE_KAFKA_IMAGE` | Qualified `ghcr.io/eighred/kanz-kafka-provision@sha256:<64-hex-digest>` |
| `NATIVE_KAFKA_MANIFEST_SHA256` | SHA256 of the exact tracked `kanz/infra/kafka/topics-job.yaml` bytes |
| `NATIVE_KAFKA_BROKER` | Intended TLS bootstrap `host:port` |
| `NATIVE_KAFKA_BROKER_SPIFFE_ID` | Exact `spiffe://kanz.internal/ns/kanz-messaging/sa/kafka` or `/sa/redpanda` |
| `NATIVE_KAFKA_CLUSTER_ID` | Independently selected exact Kafka ClusterID |
| `NATIVE_KAFKA_PRINCIPAL_ENCODING` | `spiffe-uri` (default), or explicitly qualified `redpanda-rule-escaped-uri` with the exact Redpanda peer |
| `NATIVE_KAFKA_DURABILITY_PROFILE` | `kafka-min-isr` (default), or explicitly qualified `redpanda-raft` with the exact Redpanda peer |

The CLI's `-principal` always accepts the canonical full SPIFFE principal.
`-principal-encoding` selects only its broker ACL representation. The pinned
Redpanda DN rule parser retains the slash escapes in the replacement; its actual
principal therefore contains `\/` for each `/`. The explicit Redpanda profile
uses that same representation for ACL inventory, creation and readback. The
fixture's superuser uses it too. It never changes the SPIFFE SAN, hashes an
escaped identity, adds a wildcard, adopts an observed principal, or rewrites
existing grants. Select the profile from independently reviewed broker rules;
it is not inferred from a successful TLS handshake or an authorization failure.
The actual two-tenant/SPIRE fixture must qualify this mapping before use.

Durability is also explicit. `kafka-min-isr` requires the existing Kafka setting
`min.insync.replicas=2`. Redpanda treats that setting as a no-op, so
`redpanda-raft` instead requires exact `write.caching=false` readback; it does not
invent a value for the absent Kafka property. Both profiles require RF3, at least
two distinct observed ISR members and valid leader membership. The Redpanda
release fixture additionally proves `acks=all` progress with two connected
replicas, no acknowledgment with all replicas isolated, and recovery of prior
acknowledged history. It keeps client TLS reachable during replication isolation
and disables unsafe fsync bypass. A refused produce remains unknown effects,
not proof of rollback or absence. This is disposable qualification, not backup,
power-loss or production disaster-recovery acceptance.

Before each fault-case write, bounded read-only metadata observation must match
the intended live-broker view: brokers 0/1 for the majority and 1/2 as advertised
by isolated broker 0 for the minority. Healthy and restored cases require all
three. Foreign/duplicate broker addresses or a changed ClusterID refuse; a
healthy view cannot stand in for isolation. Redpanda's
[metadata handler](https://github.com/redpanda-data/redpanda/blob/dev/src/v/kafka/server/handlers/metadata.cc)
deliberately advertises live brokers and excludes an isolated node itself.
The tenant probe also waits read-only for group-coordinator readiness before its
single positive offset commit; coordinator errors never count as ACL denials.

Sources: [Redpanda's no-op Kafka properties](https://github.com/redpanda-data/redpanda/blob/dev/src/v/kafka/server/handlers/topics/types.h),
[producer acknowledgment semantics](https://docs.redpanda.com/streaming/current/develop/produce-data/configure-producers/),
and [write caching](https://docs.redpanda.com/streaming/current/reference/properties/topic-properties/#writecaching).

The caller keeps the canonical `kanz.internal`, `kanz-messaging`, and
`kanz-services` identity scope. It creates or exactly reads back an immutable,
hash-named ConfigMap; it does not overwrite conflicting content. The complete
manifest is mounted read-only through a file `subPath`. The tool accepts only a
bounded regular, single-link, no-follow file with the expected hash, and parses
only the literal data table; it never executes the surrounding shell.

That existing table currently expands to **60 topics per non-system tenant**,
including declared DLQs and excluding the existing system-only `dlq.archiver`.
This number is an observation, not a second topic roster: the fixture derives
its expected count from the tracked table. Topic qualification and tenant versus
compute principal shapes reuse the existing `topic` and `tenantgen` owners.

The tool must receive the existing
`spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner` X509-SVID from the
Workload API. Its certificate callback checks that identity on rotation as
well as startup. TLS authenticates the selected exact canonical broker peer.
No inline key, password, credential file, or replacement CN identity is accepted.

## One bounded invocation

These Bash examples are templates for an already authorized environment; replace
all placeholders with reviewed values. Select one invocation, not an unattended
plan/apply loop. `plan` observes without writes; `apply` creates missing resources
and reads them back; `verify` requires all resources to exist without writes.

```bash
args=(
  '-manifest=<read-only-regular-manifest-path>'
  '-manifest-sha256=<exact-64-hex-manifest-digest>'
  '-tenant=<tenant>'
  '-principal=User:spiffe://kanz.internal/ns/tenant-<tenant>/sa/<service-account>'
  '-principal-encoding=spiffe-uri'
  '-durability-profile=kafka-min-isr'
  '-broker=<bootstrap-host>:<port>'
  '-broker-spiffe-id=spiffe://kanz.internal/ns/kanz-messaging/sa/kafka'
  '-expected-cluster-id=<independently-selected-cluster-id>'
  '-workload-socket=unix:///run/spiffe/spire-agent.sock'
  '-timeout=240s'
)
kanz-kafka-provision -mode=plan "${args[@]}"
# Separate approved mutation:
kanz-kafka-provision -mode=apply "${args[@]}"
# Separate readback:
kanz-kafka-provision -mode=verify "${args[@]}"
```

`tenantctl onboard` performs the same native apply for each existing tenant and
compute principal. Its Jobs have no retries (`backoffLimit: 0`), a 270-second
active deadline, and one tool work deadline of at most 240 seconds. The existing
Job observation is bounded separately (1..300 seconds, with finite API request
timeouts). It pins the returned Job UID, verifies its executable and the sole
successful pod, and accepts exactly one complete JSON receipt matching the
manifest, cluster and durability profile. A terminal Job alone is insufficient.
Native failures do not dump arbitrary container logs. Nonroot, read-only root filesystem, dropped
capabilities, SPIFFE CSI, and finite CPU/memory settings remain explicit in the
Job. These are configured bounds, not a hard OS termination guarantee.

Keep `infra/tenancy/native-receipt.jq` beside `tenantctl.sh`, and preserve the
relative `../kafka/topics-job.yaml` layout when packaging the caller. The
in-cluster example mounts the optional `tenantctl-native-topics` ConfigMap at
`/kafka`; include the validator in its script ConfigMap and supply all native
inputs explicitly. Without these files native preflight refuses. The runner identity additionally
uses `tenant-native-observer` in `kanz-messaging` for pod and log readback; no
pod mutation or access to other tenant namespaces is added. Legacy remains the
default and native offboard refuses before any lifecycle step.

Native preflight also rejects oversized namespace/compute identities, noncanonical
compute principals and invalid observation deadlines before identity or NATS effects.
Each explicit recovery invocation creates fresh Jobs and retains earlier attempts;
it never automatically retries a lost create response. Use the logged Job name/UID
and its closed receipt to investigate before rerunning the same approved inputs.

The mandatory native qualification invokes the actual script with the tracked
runner RBAC against its owned cluster. It proves both principal shapes, reruns,
manifest conflict refusal, lost Job-create ACK and explicit recovery, wrong-cluster
refusal, unchanged neighboring quotas and early offboard refusal. NATS is explicitly
manual in this fixture: successful native work must exit PARTIAL (3), never claim
full tenant onboarding. Existing NATS production acceptance is not supplied by it.

## Mismatch, partial effects, and recovery

All existing selected topics/configuration and target-principal ACLs are checked
before the first mutation. RF3, ISR of at least two distinct known replicas,
valid leader membership, partition count, cleanup, retention, and the selected
profile's exact durability setting must match. Unexpected ACLs or unknown observations refuse. The tool never
alters retention/configuration, removes ACLs, deletes topics, or expands partitions.

Every explicit Metadata response must match the selected nonempty ClusterID;
pre-create and post-create observations retain that binding. This is an
**observed estate fence**, not an atomic broker compare-and-swap or continuous
identity assurance between RPCs. There is no global atomic snapshot of topics,
configuration, and ACLs.

Kafka administration can partially commit, and a response can be lost after an
effect. Preserve the closed JSON receipt, failed Job, and intended inputs.
`partial_or_unknown` is not rollback evidence; timeout or connection failure must
not be interpreted as no effects. A plan receipt is not completed application.
After resolving the failure and independently confirming estate/input custody,
an explicitly authorized repeat invocation reads existing resources and creates
only what is missing. Successful completion requires full topic and ACL readback.
Newly acknowledged topic creates receive up to ten seconds of read-only readiness
observation within the original 240-second total deadline. Only missing topics or
incomplete leader/ISR readiness are observed again; configuration and structural
conflicts still refuse. No write request is retried. There is no automatic
destructive rollback or deletion. Existing
mismatches require separate operator handling; do not weaken the expected values
to make the check pass. Retained Jobs/ConfigMaps have no automatic cleanup here.

## Actual qualification

The existing `redpanda-identity-proof.yml` workflow retains its original
single-broker proof and adds the mandatory `native-topic-provisioning` job. That
job prepares the pinned Linux dependencies and canonical SDK, builds the actual
admin and test images, then runs from the repository root:

```bash
python3 -B tools/native-kafka-provision/qualify.py
```

Run it only through its disposable qualification environment with source/run
identity, Docker, kind/Cilium, and the pinned SPIRE/Redpanda setup available.
The three-broker fixture must prove real uniqueid-issued full SPIFFE principals,
RF3/ISR2/configuration/ACL readback, A/B topic and group isolation, admin/peer
denials, idempotency, lost-create-response uncertainty with sentinel history
preserved, real leaf renewal/reconnect, and owned cleanup. Its evidence binds the
source, manifest, images, ClusterID, complete case inventory, and three broker
capacity observations. Unsupported mapping or insufficient storage refuses;
synthetic CN certificates and relaxed broker reserves are not substitutes.

The full table expands to 231 partitions per tenant, or 693 replicas per broker
for acme, bravo and the partial-recovery tenant. Each disposable broker receives
2 GiB of process memory within a 3 GiB container limit. The sparse functional
fixture explicitly uses a 1 MiB `segment_fallocation_step`, with exact broker
configuration readback, so unused segment preallocation fits its finite 2 GiB
data volume. The manifest-derived capacity check refuses growth beyond those
budgets. Broker reserve thresholds, RF3, fsync and `acks=all` remain enforced.
The same three pod UIDs and zero container restarts must survive all cases;
replacement or eviction cannot confer acceptance. These settings qualify
functional behavior, not production capacity or throughput. See Redpanda's
[sizing guidance](https://docs.redpanda.com/streaming/current/deploy/redpanda/manual/sizing/)
and [segment allocation property](https://docs.redpanda.com/streaming/current/reference/properties/cluster-properties/#segment_fallocation_step).

Missing/skipped native qualification cannot satisfy the release requirement.
Actual Linux qualification, all 35 image builds, and explicit deployment/cutover
review remain separate from local tests. No production acceptance, disaster
recovery coverage, or historical migration completeness is conferred by this
runbook or the disposable proof.

### Imported-image diagnosis (not qualification)

The existing Redpanda workflow has a non-main, manual `image-diagnostics` mode.
It builds the same two source-stamped images and observes their imported kind/CRI
content graph using the same bounded verifier. The admin exits on an unsupported
mode before Workload API setup; the probe explicitly skips without its fixture
mode. These Jobs have no credentials, identity/manifest mounts or broker inputs;
no SPIRE or broker is created. Only fixed predicate codes, graph progress and
owned cleanup observations are retained. Such receipts always carry
`diagnosticOnly=true` and `qualification=false`; they cannot satisfy the full
17-case inventory or the two required release jobs. PR, main and default manual
runs retain both full qualifications. No failed diagnostic authorizes a retry,
provisioning, or a change to the image identity predicates.

## Opt-in GitOps system topic bootstrap

Render the native PostSync Job for the target estate using the existing
`tools/native-kafka-provision/system_bootstrap.py` renderer with Python and
PyYAML. Supply `--image` (published provisioner digest), `--broker` (TLS
host:port), `--peer` (exact broker SPIFFE ID), `--cluster` (exact observed
cluster ID), and `--profile` (matching durability profile). The renderer
uses the tracked system topic table and emits a digest-named immutable
ConfigMap, the existing provisioner service account, and a named PostSync Job.

In the estate's reviewed GitOps source, replace the legacy topics Job with
the rendered resources in the same application. Exclude the legacy
`topics-job.yaml` from that application's sync while retaining its tracked
table as the renderer's input. Review the rendered application for a single
system topic owner before syncing. The hook requires a ready SPIRE CSI
installation, a provisioner SVID and a broker policy granting that identity
system topic administration over the pinned TLS listener.

The hook creates missing system topics and verifies exact configuration,
partitions and durability. Existing mismatches fail before writes. Resolve
the drift under a separate reviewed operation, then sync again. Argo's
BeforeHookCreation policy replaces the Job on the next sync. The next run
checks existing topics and reports zero writes. A failed or lost write
acknowledgement requires readback before the next explicit sync.

The disposable Redpanda fixture executes the rendered Job, recreates it using
the same name to exercise the hook lifecycle, changes a retention expectation
to exercise refusal, and verifies the original configuration afterward.
This checks the real broker and Job behavior. Argo controller reconciliation
and estate activation require separate verification.

### Argo controller qualification

The disposable native broker workflow installs digest-pinned Argo CD Core
in the same owned kind estate as the three Redpanda brokers and SPIRE. A
cluster-local, read-only Git server serves fixture commits containing the
rendered PostSync hook. The harness submits full Application sync operations
through Kubernetes and waits for the controller's operation result and
PostSync phase. The same commit is synced twice and the Job UID must change.
A separate commit presents a conflicting retention expectation and must
fail the sync with a zero-write receipt. A final verification commit checks
the original topic settings. This qualifies the controller path in a
disposable estate and records revision, phase and Job UID for each sync.

### Pinned activation and rollback qualification

After the initial hook proof, the fixture writes an acknowledged synthetic
record into the unprefixed order topic. It activates a distinct Git revision
with a changed hook execution budget, verifies all system topic settings,
and writes a second acknowledged record. It then syncs back to the exact
initial Git commit. Both revisions use the same source-bound native image
and topic manifest. The rollback must verify all topic settings with zero
topic and ACL writes and read both retained records from the same broker
cluster. At each completed sync the fixture inventories the Application and
system writer Jobs and requires exactly one owner. Each operation must use
a fresh Job UID.

This rehearses a Git revision rollback between compatible native hook
configurations in a disposable estate. Retiring the legacy JVM caller still
requires qualification of the transition from the legacy owner and the
estate-specific fallback.
