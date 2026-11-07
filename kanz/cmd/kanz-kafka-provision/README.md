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
Job wait is bounded separately. Nonroot, read-only root filesystem, dropped
capabilities, SPIFFE CSI, and finite CPU/memory settings remain explicit in the
Job. These are configured bounds, not a hard OS termination guarantee.

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
There is no automatic destructive rollback, deletion, or retry policy. Existing
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
