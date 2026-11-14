# Grafana Kubernetes qualification

`grafana-verification.yml` verifies #85 on a new, named kind/Cilium cluster. It
reuses the existing checksum-pinned installer and cluster constructor; the
constructor rejects existing contexts. Run the workflow on the reviewed PR and
again on its actual merged main revision. The JSON receipt binds source/tree,
run/attempt, public input hashes, declared and loaded images, Pod identities,
all 11 dashboard targets, every required case and verified cluster deletion.
No production endpoint, credential or existing cluster is an input.

The proof requires a clean Git HEAD equal to the independently supplied
`GITHUB_SHA`, with the Actions run and attempt bound into its receipt. Before
creation it checks the chosen cluster name is absent and exclusively writes a
nonce/source/tree/run/attempt owner record in `RUNNER_TEMP`. Normal and interrupted
cleanup validate that record against the Actions identity and delete only its
exact cluster name. Another cluster with the same prefix must remain present;
neither cleanup path enumerates matching names as deletion targets. Owner-record
I/O requires Linux no-follow, regular-file and exclusive-create semantics.

The harness applies the committed Grafana and Prometheus manifests, generated
rules ConfigMap and dashboard. It creates a random dummy admin Secret separately
at runtime through stdin. It never prints the password, authorization headers,
Secret contents, Pod logs or raw API responses. A separate namespace without that
Secret must produce an owned `FailedMount` and no started Grafana container.
Running Grafana must prove its public mount hashes and writable data/tmp paths
with UID 472, authenticated provisioning, and unauthenticated dashboard refusal.

A dummy counter exporter supplies the five existing SLOs through the committed
Kubernetes discovery configuration. Known fixed increments give burn multiples
100, 5, 40, 30 and 150; no trading or production metrics are used. Every committed
target is evaluated at one instant through the real Grafana Prometheus backend,
the provisioned datasource proxy and direct Prometheus. Each result must be
nonempty, finite and agree with the independent counter oracle. The fresh TSDB
must expose incomplete 28d coverage and the dashboard's `NOT MEASURED` budget
sentinel. This does not invent 28 days of history or certify budget retention.

The source policy selects Grafana only. The proof checks:

- Grafana can query same-namespace Prometheus on 9090.
- Grafana cannot reach a different service on 9090.
- An untrusted pod cannot reach Grafana on 3000.
- Removing that exact policy makes both refused paths work; restoring it refuses
  them again, while Cilium reports its loaded enforcing mode.

Network refusal requires a nonce-bound start/result marker from a completed
`wget` probe in the explicitly selected container. Only the target tool's exact
timeout is a denial; kubectl/API/container failures, missing tools, unexpected
diagnostics and incomplete markers refuse the proof. In particular an API-side
"timed out waiting for the condition" cannot count as CNI enforcement.

These observations do not claim global Prometheus ingress isolation. KEDA,
Rollouts and GameDay readers retain their existing paths. The broader namespace
audit remains #232; the existing real-container dashboard proof remains valid.

Offline controls run on Linux, including the real no-follow owner-record check:

```sh
python3 -B -m unittest discover -s tools/grafana_proof -p 'test_*.py'
```

Actual Kubernetes proof additionally requires the workflow's pinned
kind, Helm, kubectl and Cilium inputs plus disposable Docker capacity. An offline
test, a green health endpoint or an incomplete receipt cannot close #85.
