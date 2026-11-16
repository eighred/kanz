# NATS GitOps and PVC verification

This #92 proof creates its own Linux kind/Cilium/SPIRE/Argo CD cluster. It accepts
no existing Kubernetes context and uses no production identity, credential or
order. Canonical NATS manifests keep all three brokers, the data PVC, SPIRE CSI,
mTLS mapping and account permissions. Only the disposable SPIRE authority's store
uses the existing development fixture's memory volume.

Argo reconciles the committed `kanz/infra/nats` directory using the production
ApplicationSet's directory exclusions. Canonical tenancy is installed first as
the prerequisite that tenant provisioning owns; the bootstrap Job is created
only by Argo. No bootstrap command is executed manually. PR runs must resolve
the advertised PR merge ref to the exact checked-out test-merge SHA; main runs
use the actual push SHA. A moving ref fails the proof.

The receipt requires two successful revision-bound PostSync operations with
different hook Job UIDs. Each operation is submitted once. One unchanged
600-second deadline covers submission and read-only polling of both its exact
revision/token-bound Argo result and the actual hook's completed state. A typed
fixed-name JobList read treats an empty list as pending absence; read failures,
foreign annotations/tracking, malformed state, a failed hook or a replacement of
the first fresh UID refuse. The pre-operation UID, including before the first
sync, can never qualify as fresh completion. Argo v3.6.1's default annotation
tracking binds the Job to this fixture Application; no tracking-policy override
is applied. The fixed raw API path preserves JobList typing that kubectl's
generic JSON printer otherwise rewrites to List.

The receipt records only closed per-operation observations: the unchanged budget,
poll count, whether Argo success preceded actual-hook completion, whether a stale
UID was observed, and final joint success. ArgoSucceededPendingHook is an observed
availability transition, not a diagnosis of a previous run or a controller
guarantee. The second sync follows deletion of the empty, designated
OBSERVABILITY stream. All 18 stream configurations are derived from the canonical
script and checked after creation, restoration and broker replacement. There is
no fallback stream list, storage override or single-replica substitute.

The module-owned Go diagnostic, `kanz/tools/nats_gitops_proof`, builds a generated `market.v1.MarketDataEvent` inside the
canonical `EventFrame`, calls the shared live envelope validator, rejects a
conflicting FACT idempotency key, and checks the generated protobuf round trip.
The actual SPIRE-issued market-ingest identity publishes that synthetic quote
using the existing Go SPIFFE/JetStream client and must receive the exact first
MARKET PubAck. The source-bound static diagnostic runs inside the owned writer
Pod; it never exports an SVID and can publish only the validated prepared fixture.
Its main container receives the existing SPIFFE CSI socket as a read-only mount;
the canonical bootstrap/inspector CLI continues to use init-materialized PEM files.
The observed writer Pod spec must prove that mount before publication is attempted.
Its command errors use closed categories. The canonical CLI v0.1.5 has no
publish `--jetstream` flag and reads captured-output stdin only with a specific
publish flag; a core publish exit alone therefore cannot be an acknowledgement.
The exact [pinned CLI source](https://github.com/nats-io/natscli/blob/v0.1.5/cli/pub_command.go)
defines that distinction. No broker image or permission change supplies the repair.

The broker's bytes, subject, headers, sequence and timestamp must survive a cold
restart of every NATS process. The fixture scales the unchanged StatefulSet to
zero, observes zero broker Pods and all three original bound claims, then
restores its canonical three replicas. Every Pod UID must change while every
PVC/PV identity remains. This discriminates persistent recovery from replication
by a still-running peer. The operator fixture uses only the
existing bootstrap identity's JetStream management grant.

After replacement, a single 180-second deadline covers read-only stream listing,
all 18 exact configuration reads and exact retained FACT readback. Every command
is limited to 15 seconds or the remaining budget. Missing stream names and closed
timeout/listener categories may converge within that budget; unexpected streams,
observed configuration differences, changed FACTs and authorization refusals fail.
No additional Argo sync or bootstrap is used. Pod readiness alone does not attest
stream recovery. The receipt projects the first complete inventory sample, latest
observed/missing names, differing field names, attempts, elapsed time and deadline
expiry. These observations distinguish availability symptoms from proven drift;
they do not establish that a missing stream was caused by leader election.

`nats-gitops-verification / postsync-pvc` runs the real proof on PR and main.
Offline `unittest` tests discriminate false receipts and changed source, hook,
stream, frame, PubAck and cold-storage assertions. The Go owner additionally
refuses a changed checksum, source, header or resigned non-FACT before broker
access. Runtime receipts contain synthetic data,
source/tree hashes, run identity and actual image IDs, but no raw Pod logs, keys,
tokens or kubeconfig. Cleanup must remove the exact cluster created by the proof.
This is disposable estate acceptance, not a Tokyo rollout or capital authority.

The mandatory source inventory is 26 Python test methods in `test_proof.py` and
the two Go tests in `kanz/tools/nats_gitops_proof/main_test.go`. The four recovery
controls cover missing/transient-then-recovered inventory, permanent missing or
unavailable inventory/FACT at the deadline, immediate configuration/FACT/auth
refusal, and one shared deadline across all inventory commands. Receipt mutations
also refuse missing or forged convergence evidence. The runtime receipt requires
all 16 ordered cases: canonical-source, first-postsync, canonical-runtime,
stream-inventory, writer-workload-api-socket, canonical-fact, empty-stream-removed,
same-revision-postsync, stream-restored, all-brokers-stopped, all-claims-retained,
all-brokers-replaced, pvc-preserved, post-cold-stream-inventory, fact-preserved,
cleanup. Eleven additional joint-hook controls include actual sync-method calls
through fixed fake APIs and refuse missing, stale, foreign or late evidence, as
well as a completed Job without current-operation completion. They also validate
the typed inventory and close the new observation fields. Unit controls do not
establish actual broker recovery.
