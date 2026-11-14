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
different hook Job UIDs. The second follows deletion of the empty, designated
OBSERVABILITY stream. All 18 stream configurations are derived from the canonical
script and checked after creation, restoration and broker replacement. There is
no fallback stream list, storage override or single-replica substitute.

The Go diagnostic builds a generated `market.v1.MarketDataEvent` inside the
canonical `EventFrame`, calls the shared live envelope validator, rejects a
conflicting FACT idempotency key, and checks the generated protobuf round trip.
The actual SPIRE-issued market-ingest identity publishes that synthetic quote.
The broker's bytes, subject, headers, sequence and timestamp must survive a new
NATS Pod UID with unchanged PVC/PV identity. The operator fixture uses only the
existing bootstrap identity's JetStream management grant.

`nats-gitops-verification / postsync-pvc` runs the real proof on PR and main.
Offline `unittest` tests discriminate false receipts and changed source, hook,
stream, frame and storage assertions. Runtime receipts contain synthetic data,
source/tree hashes, run identity and actual image IDs, but no raw Pod logs, keys,
tokens or kubeconfig. Cleanup must remove the exact cluster created by the proof.
This is disposable estate acceptance, not a Tokyo rollout or capital authority.
