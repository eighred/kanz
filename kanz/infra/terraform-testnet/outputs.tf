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
