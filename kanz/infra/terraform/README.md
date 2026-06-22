# INFRA-01a — cloud foundation (Terraform)

The cloud substrate the platform runs on, as code: a multi-AZ VPC, an EKS
cluster with stateless + stateful node groups, and the Object-Lock backup
bucket. **Terraform owns cloud resources; GitOps (INFRA-01b) owns the k8s
workloads** — including the Kafka/NATS/Postgres data planes, which run in-cluster
on this foundation rather than as managed services (consistent with
`infra/kafka`, `infra/nats`, `infra/dr/postgres`).

| File | Provisions |
|---|---|
| `vpc.tf` | multi-AZ VPC, one NAT per AZ (an AZ loss strands no other), EKS-tagged subnets |
| `eks.tf` | EKS cluster + a `general` (stateless) and a tainted `stateful` (brokers/DB) node group, each spread across all AZs |
| `storage.tf` | COMPLIANCE Object-Lock + KMS backup bucket (DR-01b WAL/PITR + AUDIT-01b WORM) and the backup IRSA role |
| `versions.tf` / `variables.tf` / `outputs.tf` | pins, per-env inputs, outputs for the GitOps layer |

## Why this shape

- **AWS**, because the platform already targets S3 for DR-01b backups / AUDIT-01b
  export — one provider, not two. A GCP/Azure port swaps the modules; the
  network → cluster → storage layering is provider-agnostic.
- **≥3 AZs + per-AZ NAT**, so the INFRA-01d topology spread holds quorum and the
  INFRA-01e AZ-kill leaves the system serving.
- **Object Lock COMPLIANCE**, so backups can't be deleted/overwritten before
  retention even by an admin — "provable RPO" needs immutable backups.

## Use

One state + tfvars per environment (and per region for the DR-01 pair):

```sh
terraform init -backend-config=backends/primary.hcl
terraform apply -var-file=env/primary.tfvars     # region=…, environment=primary
# DR region:
terraform apply -var-file=env/dr.tfvars          # region=…, environment=dr (non-overlapping vpc_cidr)
```

Then point Argo CD (INFRA-01b) at the cluster and annotate the DR-01b
ServiceAccounts with `backups_irsa_role_arn`. The `backends/` and `env/` files
(state bucket, region, CIDRs) are environment-specific and live with the
deployment config, not in this module.
