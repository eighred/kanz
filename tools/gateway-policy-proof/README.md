# Rendered Tokyo gateway ingress qualification

This development-only fixture renders `kanz/infra/overlays/testnet-tokyo` and
applies **all** resulting NetworkPolicy objects unchanged to a fresh, owned
kind/Cilium cluster. It never accepts an existing context or applies an overlay
to Tokyo. The shared feed-identity installer and cluster helper retain their
reviewed kind node and Cilium pins. No workload image, credential, domain record,
Vault binding or production service is deployed by this proof.

Synthetic nonroot, read-only Python listeners use the actual rendered deployment
labels and API/ops ports for accounting, audit, compliance and gateway. The
fixture measures both direct Pod IP and Service IP paths. Its 76 mandatory cases
include pre-policy connectivity from each subsequently denied peer, gateway API
access, observability ops access, denied direct BFF/untrusted/observability API
access, and a gateway label in the wrong namespace. BFF-to-gateway access remains
allowed while an unrelated caller is denied. Each negative requires three
independent connect timeouts, bracketed by successful HTTP controls on the same
destination and port. Connection refusal, reset, or a timeout after connection
is established is not accepted as a policy denial.

BFF-to-upstream denial is a whole-union result: the rendered BFF egress rule can
also block that path. Untrusted same-namespace and cross-namespace callers have
no fixture egress restriction and independently exercise destination ingress.

The fixture removes only the accounting ingress policy in the disposable cluster
and requires the formerly denied caller to succeed. It then restores the exact
captured object, verifies the full additive policy spec inventory, and requires
denial again. Readback normalizes only empty ingress/egress slice omission and
the Kubernetes TCP port default; selector presence and all policy names/specs
remain compared. Kubernetes policy convergence has a bounded settling phase; its
observations do not count as acceptance. Recorded cases always use three fresh
observations. Namespace-wide policy gaps outside these selected services are
unchanged; this does not establish HTTP authentication, financial authorization,
RLS, actual service recovery or safety against a principal permitted to relabel
pods. No real tenant API requests are made.

The workflow runs offline refusal tests, the actual cluster proof, and an
independent checker bound to checkout SHA/tree and workflow run/attempt. The
artifact contains rendered policy hashes/inventory, fixed case results and
cleanup status; no application payloads, credentials or raw pod logs. Cleanup
uses only the fresh recorded cluster nonce and confirms absence before success.
An interruption cleanup step removes that same cluster; it cannot turn a failed
or incomplete proof into successful evidence.

Local logic checks:

```sh
python3 -B -m unittest discover -s tools/gateway-policy-proof -p 'test_*.py'
```

Actual acceptance is the Linux `gateway-policy-proof` / `gateway-isolation` job.
It requires Docker, the pinned installer artifacts, and a disposable runner.
There are no dependency-skipping branches. A passing kind/Cilium result is
developmental policy enforcement evidence, not authorization for a live rollout
or a claim that every other CNI implementation has been exercised.

## Controlled Tokyo four-policy transaction

`tools/Invoke-TokyoGatewayPolicy.ps1` is a fixed-estate operator entrypoint, not a
generic Kubernetes apply tool. Its default performs inventory qualification and
server-side dry runs; `-Apply` requests the four reviewed policy writes. Neither
mode may run from an unmerged checkout or before the canonical exact-main CI
policy succeeds. The wrapper fixes Tokyo account, instance, region, operator
profile/source chain and assumed-role ARN. The engine independently checks the
existing kube-system UID. Publishing this source does not authorize live use.

Host acceptance/image resolution and the remote engine launch Python in isolated
mode. A closed loader compiles the approved repository modules from checked
source bytes; repository directories never enter the import search path. Ignored
bytecode caches and local standard-library shadows therefore cannot replace
those inputs. Existing caches are preserved. The remote seven-file bundle retains
the 20 KB encoded and 512 KiB expansion limits; this is source custody, not an
operating-system sandbox or attestation of the installed Python toolchain.
The original JSON/source bytes use one shared XZ codec (LZMA2 preset 6,
1 MiB dictionary, CRC64). Isolated host and remote Python must provide `lzma`;
missing support refuses before remote scratch creation, with no installation or
fallback. The decoder allocation budget is 16 MiB, not a total Python RSS limit.
Decoding requires complete single-stream
EOF with no trailing bytes, and retains the 64 KiB per-file limit. The complete
SSM parameter JSON is also limited to 30,000 UTF-8 bytes/characters to reserve
space for the Windows native command line. The Linux transaction fixture runs
the same decoder and malformed-stream controls as a mandatory additional case.

Probe custody binds UID and nonce before associating any observed address,
including addresses seen before CRI/spec qualification. Receipts retain the
bounded union of observed canonical addresses and owned UIDs after deletion.
Malformed or overflowing history remains uncertain even after a later normal
read. Cleanup still deletes a demonstrably owned Pod by UID/resourceVersion,
but cannot report verified cleanup with unresolved address custody. Endpoint
checks cover all observed addresses and owned UID references, not unknowable
historical addresses. Replacement objects are never deleted.

The IPv6 control creates a canonical RFC 3849 address in an EndpointSlice for a
fresh nonce Service name whose absence is checked before and after the API
readback. It has no targetRef or Service and sends no IPv6 traffic. The actual
canonical metadata must refuse cleanup against an equivalent expanded observed
input, while explicit unready metadata passes the nonparticipation check. The
artifact binds the slice UID, absent Service, exact observed shape and literal
comparison counterexample as the twentieth ordered case; all prior nineteen
cases and seventy-six policy observations remain mandatory.
Expanded *stored* addresses and mapped forms are unit/legacy coverage only:
[pinned discovery validation](https://github.com/kubernetes/kubernetes/blob/v1.36.4/pkg/apis/discovery/validation/validation.go)
uses [strict canonical IPv6 validation](https://github.com/kubernetes/apimachinery/blob/v0.36.4/pkg/util/validation/ip.go)
for new slices. This control makes no dual-stack, IPv6 Cilium or live-estate claim.

The owned set is the three shared API ingress policies and
`allow-gateway-runtime-egress`. The only accepted older egress shape omits the
two exact accounting:8101 and audit:8102 peers. The entire services policy union
must otherwise match the rendered overlay. Policies outside that namespace,
Services and the seven selected runtime Pods are captured and must remain
unchanged. The five governed applications must match the rendered Deployment
container/init-container image pins, template labels and selectors; each live
Pod is bound through its ReplicaSet to that Deployment's UID. This source/spec
and controller check does not independently resolve private ECR runtime config
bytes. NATS/Redis are observed baselines, not source-version qualifications.
A write uses creation or UID/resourceVersion-bound replacement.
Dry-run uses transient scratch removed on exit, creates no probes, and reports a null receipt.
Existing user metadata is retained. For apply, an append-only private receipt is fsynced
before each side effect; a lost response is reconciled against the marker,
desired object and original UID. Rollback restores semantic original objects
with current CAS versions, or deletes only newly created objects with both UID
and resourceVersion preconditions. A concurrent change refuses automatic
rollback. Private receipts remain under the reported `/var/tmp` directory,
including on interruption; there is no force/retry or automatic receipt deletion.

Live probe Pods have no service-account token, listener or domain operation.
They perform TCP connects only, sending no HTTP or principal headers. Their
custom readiness gate is never satisfied. Before use the engine checks actual
Running-but-unready state, exact containers and immutable image identity, rejects
any matching Service with `publishNotReadyAddresses`, and rejects ready/unknown
EndpointSlice entries for probe IPs. Only required app and ownership labels are
used, never a ReplicaSet hash. Cleanup checks nonce/UID/resourceVersion and
verifies Pod/namespace deletion and removal of every probe endpoint address.
No production Pod is edited and no ephemeral container is installed.

The public image resolver authenticates anonymously for the fixed Python
repository only. It hashes the fixed root, any selected linux/amd64 child and
config bytes, then projects digests/platform only. The current pin resolves to a
direct OCI manifest, not an index. Its config digest is compared with the typed
CRI image inspection and repository digest, separately from the Pod's imageID.
No Docker Desktop or credential-helper dependency is introduced. Registry tokens
and raw config remain subprocess-local. Redirect handling follows the
[OCI authorization rule](https://github.com/opencontainers/distribution-spec/blob/v1.1.1/spec.md)
and only the documented [Docker registry/CDN hosts](https://docs.docker.com/desktop/enterprise/allow-list/);
it never forwards authorization across hosts.

The same required workflow also constructs a second fresh kind/Cilium cluster
for `transaction_fixture.py`. Eighteen mandatory cases exercise each real API
write/finalization response-loss boundary, dry-run nonmutation, actual Deployment
UID lineage, zero-replica controller adoption refusal, four-policy
success, refusal to overwrite a concurrent edit, actual neverReady endpoint
nonparticipation, actual matching-Service refusal and UID-bound cleanup. Its
live TCP probe routine also runs against synthetic listeners under the exact
rendered policies, with successful controls bracketing every timeout denial. Its
checker binds checkout/tree, run/attempt, rendered union and independently
resolved image identity. A fixture-only empty Service must produce a controller-owned
EndpointSlice whose actual API JSON contains `endpoints: null`; its UID bindings
and exact projected shape are mandatory evidence. The slice remains present while
real probe checks and cleanup execute. Malformed inventories and unknown readiness
for any matching probe still refuse. The nullable list matches the pinned
[Kubernetes API type](https://github.com/kubernetes/api/blob/v0.36.4/discovery/v1/types.go#L49-L54)
and [empty-placeholder controller path](https://github.com/kubernetes/endpointslice/blob/v0.36.4/reconciler.go#L241-L254).
This is developmental transaction evidence, not Tokyo
rollout acceptance. Linux execution is required; local Windows receipt tests
explicitly skip unsupported no-follow directory semantics.

The policy adapter uses fixed, validated raw API POST/PUT paths because ordinary
kubectl replace rewrites an existing last-applied annotation even without
`--save-config`. User annotations remain part of exact semantic comparison.
Raw dry runs use the literal `?dryRun=All` query parameter and verify the returned
policy; the ordinary CLI dry-run flag does not govern raw requests. The added
transaction control establishes the old annotation mutation on a disposable
policy, then requires exact custom metadata preservation across raw create,
replace, finalization and all eight rollback cases, unchanged dry-run inventory,
and refusal of stale resourceVersion and wrong UID updates. This source repair
does not reconcile or authorize mutation of any retained live transaction.
