# Operator identities and the manual halt invocation

`halt-identity.yaml` owns the `kanz-operator` namespace, its SPIRE issuance label,
the `kanz-halt` ServiceAccount and the halt tool's broker/DNS egress policy.
`cli-identities.yaml` owns the other operator ServiceAccounts and their egress
policies. The GitOps `operator` component continuously reconciles both files;
the `kanz` AppProject permits their destination namespace.

`halt-job.yaml` contains only the per-invocation Job. Argo excludes that exact
file, so syncing identities, deleting a completed Job or rolling identities back
cannot invoke the brake. Keep the file separate: putting an ordinary identity
there would remove that identity from reconciliation. Adding a halt Job to a
different file would place an invocation in automated sync.

The existing manual Job path is retained. For an estate that has not yet synced
the identity component, apply the prerequisites before an independently
authorized invocation:

```sh
kubectl apply -f kanz/infra/operator/halt-identity.yaml
kubectl apply -f kanz/infra/operator/cli-identities.yaml
```

Copy `halt-job.yaml`, set the attributed `--by` and `--reason` arguments for that
invocation, then use the existing manual apply/log/delete procedure in its
header. This split changes delivery of identities; it grants no additional NATS
permission, signatory authority or permission to resume trading. Existing
incident, mandate and admission requirements still apply.

`TestOperatorIdentitiesSyncWithoutInvokingHalt` checks the complete identity
surface, exact Job exclusion, single source owner, namespace and AppProject.
`TestOperatorGitOpsGuardRejectsBoundaryMutations` checks that a syncable halt,
an excluded/deleted identity, a mixed invocation file or another automatic Job
cannot pass. The disposable Argo fixture in
[`tools/operator_gitops_proof`](../../../tools/operator_gitops_proof/README.md)
separately verifies actual reconciliation and rollback. Source checks alone
do not establish deployed identity issuance or operational acceptance.
