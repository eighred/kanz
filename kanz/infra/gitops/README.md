# Argo CD ownership and bootstrap

The primary ApplicationSet targets the cluster in which Argo CD runs. Tokyo
testnet has a separate workload installer. Determine which process owns the
destination objects before applying a bootstrap or enabling automated sync.

## Primary ApplicationSet

The root Application selects exactly two files from this directory. The
AppProject admits the source repository, destination namespaces and resource
kinds. The ApplicationSet generates one Application for each component in the
single `primary` environment at `https://kubernetes.default.svc`.
[Bootstrap](bootstrap.yaml) · [AppProject](projects.yaml) · [ApplicationSet](applicationset.yaml)

| Application suffix | Desired state | Destination namespace |
|---|---|---|
| `messaging-nats` | NATS manifests and the rerunnable stream bootstrap hook | `kanz-messaging` |
| `messaging-kafka` | Kafka manifests | `kanz-messaging` |
| `workloads` | Workload manifests, including rendered tenant OMS manifests | `kanz-services` |
| `security` | Security manifests | `kanz-services` |
| `operator` | Operator ServiceAccounts and egress policies | `kanz-operator` |
| `observability` | Prometheus, rule ConfigMap, Grafana and node-exporter manifests | `kanz-observability` |

Each generated Application follows `main` and requests automated prune and
self-heal. A removal in Git can delete an owned resource in the cluster.
The ApplicationSet excludes specific files by name. It reconciles operator
identities while excluding the manual `halt-job.yaml` invocation from sync,
self-heal and pruning. [Operator instructions](../operator/README.md)
The architecture guards
check component coverage, AppProject destinations and the provisioning hook.
[Sync coverage](../../test/arch/gitops_sync_completeness_test.go) · [Provisioning Jobs](../../test/arch/gitops_provisioning_job_test.go)

Bootstrap only in a designated primary cluster with Argo CD already installed
and the component inventory assigned to it. From this directory, select that
cluster explicitly and inspect the Applications in the same context:

```sh
kubectl --context "${PRIMARY_CONTEXT:?set the approved primary context}" apply -f bootstrap.yaml
kubectl --context "$PRIMARY_CONTEXT" -n argocd get applications.argoproj.io
```

The preview ApplicationSet has a separate opt-in lifecycle. A DR deployment
requires a distinct destination and a reconciler that remains available in the
recovery region. The DR runbook owns promotion of regional PostgreSQL
replicas. [Preview](preview-applicationset.yaml) · [DR runbook](../dr/README.md)

## Tokyo testnet

The workload installer renders the Tokyo overlay, verifies the annotated
release and retained ECR digests, and uses the `kanz-bootstrap` field manager.
Argo Rollouts manages canary progression for the risk-engine Rollout. The
Terraform foundation, data-plane bootstrap, Vault material and break-glass
halt invocation have separate owners. The primary ApplicationSet manages its
operator identities in its own destination. [Operator scope](https://github.com/eighred/kanz/issues/1342)
[Tokyo workload runbook](../overlays/testnet_tokyo/README.md) · [Data-plane runbook](../overlays/testnet_tokyo/data/README.md)

The primary ApplicationSet's raw workload base and the Tokyo overlay declare
the same objects, including `Deployment/oms` and `Rollout/risk-engine` in
`kanz-services`. Applying the primary bootstrap on Tokyo would give Argo CD
and the installer competing desired states for those objects.

The intended Tokyo reconciler is Argo CD with a dedicated Application scoped
to a reviewed Tokyo release. Before transferring any objects, compare the
rendered group, kind, namespace and name with the installer's inventory and
the other Applications on the destination. Retire the previous apply and
rollback path for those objects at handoff. Keep Secret material, one-shot
operations and data recovery with their existing owners.

Signed release promotion verifies a specific artifact and its attestations.
The checked-in image policy selects GHCR images. Tokyo uses ECR digests and
needs evidence that admission at that destination rejects unsigned and
wrong-source images. The handoff also requires observed controller drift
repair, fresh-node promotion and rollback to a retained release with state
preserved. [Promotion](../../../tools/signed_promotion/README.md) · [Image policy](../security/admission/cluster-image-policy.yaml) · [Controller acceptance](https://github.com/eighred/kanz/issues/1547)
