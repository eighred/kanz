output "instance_id" {
  description = "Instance identifier used with AWS Systems Manager Session Manager."
  value       = aws_instance.node.id
}

output "public_ip" {
  description = "Stable outbound IP for exchange testnet allowlists. No inbound security-group rule exists."
  value       = aws_eip.node.public_ip
}

output "availability_zone" {
  description = "Selected Tokyo availability zone."
  value       = local.availability_zone
}

output "ssm_start_session" {
  description = "Administrative entry point; SSH is intentionally unavailable."
  value       = "aws ssm start-session --profile ${var.aws_profile} --region ${var.region} --target ${aws_instance.node.id}"
}

output "testnet_ecr_registry" {
  description = "IAM-authenticated registry used by the Tokyo capital-path workload."
  value       = "${data.aws_caller_identity.current.account_id}.dkr.ecr.${var.region}.amazonaws.com"
}

output "github_ecr_publish_role_arn" {
  description = "Set this non-secret ARN as the AWS_TESTNET_ECR_ROLE_ARN GitHub Actions variable."
  value       = aws_iam_role.github_ecr_publish.arn
}

output "capital_path_ecr_repositories" {
  description = "Immutable ECR repositories the node is permitted to pull."
  value       = { for name, repository in aws_ecr_repository.capital_path : name => repository.repository_url }
}

output "recovery_bucket" {
  description = "Osaka Object-Locked bucket for encrypted testnet recovery artifacts."
  value       = aws_s3_bucket.recovery.id
}

output "recovery_kms_key_arn" {
  description = "Osaka KMS key required for recovery artifact encryption and restore."
  value       = aws_kms_key.recovery.arn
}
