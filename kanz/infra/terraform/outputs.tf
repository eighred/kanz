# INFRA-01a — outputs the GitOps layer (INFRA-01b) and operators consume.
output "cluster_name" {
  value = module.eks.cluster_name
}

output "cluster_endpoint" {
  value = module.eks.cluster_endpoint
}

output "cluster_oidc_provider_arn" {
  value = module.eks.oidc_provider_arn
}

output "vpc_id" {
  value = module.vpc.vpc_id
}

output "availability_zones" {
  description = "AZs the cluster spans — the INFRA-01d topology-spread domain."
  value       = local.azs
}

output "backups_bucket" {
  value = aws_s3_bucket.backups.bucket
}

output "backups_irsa_role_arn" {
  description = "Annotate the DR-01b CloudNativePG ServiceAccounts with this (eks.amazonaws.com/role-arn)."
  value       = module.backups_irsa.iam_role_arn
}
