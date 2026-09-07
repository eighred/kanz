# Tokyo capital-path workload overlay

This overlay is the immutable image lock for the cost-bounded single-node
testnet. It reuses the canonical workload manifests, replaces only their image
references with same-account Tokyo ECR digests, removes their GHCR pull-secret
dependency, and scales workloads to one replica. It does not weaken application
risk, compliance, mandate, mTLS, tenant, or secret-provider boundaries.

Render and inspect before applying:

```powershell
kubectl kustomize .\infra\overlays\testnet-tokyo `
  --load-restrictor LoadRestrictionsNone > $env:TEMP\kanz-testnet-tokyo.yaml
kubectl apply --server-side --dry-run=server -f $env:TEMP\kanz-testnet-tokyo.yaml
kubectl diff -f $env:TEMP\kanz-testnet-tokyo.yaml
```

Do not apply until the namespace, SPIFFE registrations, Vault
`SecretProviderClass` objects, NATS, Postgres migrations, and Argo Rollouts CRD
are healthy. The digest lock identifies build commit
`a3dea5a5fb0f5d34229390911b55165f5fdb4c5b`; changing it requires resolving all
ten workload/migration images from one successful main build and rerunning the
overlay architecture guard.

The `kanz-capitalpath` certifier image is intentionally not a standing workload.
Pass its ECR digest separately to `infra/dr/run-capitalpath-certification.sh`
only after the DR attestation and order safety gates are green.
