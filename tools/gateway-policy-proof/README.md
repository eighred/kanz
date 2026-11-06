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
