variable "aws_profile" {
  description = "MFA-backed AWS CLI profile used by Terraform."
  type        = string
  default     = "kanz-platform"
}

variable "region" {
  description = "The approved testnet region. This stack deliberately refuses every other region."
  type        = string
  default     = "ap-northeast-1"

  validation {
    condition     = var.region == "ap-northeast-1"
    error_message = "Issue #71 single-node certification is approved only for ap-northeast-1 (Tokyo)."
  }
}

variable "name" {
  description = "Resource name prefix."
  type        = string
  default     = "kanz-testnet-tokyo"

  validation {
    condition     = can(regex("^kanz-testnet-[a-z0-9-]+$", var.name))
    error_message = "The name must start with kanz-testnet- and contain only lowercase letters, numbers, and hyphens."
  }
}

variable "instance_type" {
  description = "Approved x86 instance size for the single-node capital-path testnet."
  type        = string
  default     = "t3a.large"

  validation {
    condition     = var.instance_type == "t3a.large"
    error_message = "This cost-bounded stack is intentionally pinned to t3a.large."
  }
}

variable "root_volume_gib" {
  description = "Encrypted gp3 OS volume size."
  type        = number
  default     = 20

  validation {
    condition     = var.root_volume_gib == 20
    error_message = "The reviewed cost model permits exactly 20 GiB for the root volume."
  }
}

variable "data_volume_gib" {
  description = "Encrypted gp3 volume for k3s, container images, Postgres, NATS, Vault, and SPIRE state."
  type        = number
  default     = 80

  validation {
    condition     = var.data_volume_gib == 80
    error_message = "The reviewed cost model permits exactly 80 GiB for the data volume."
  }
}

variable "budget_email" {
  description = "Email subscriber for the mandatory USD 50/75/90/100 monthly AWS budget alarms. Supply through TF_VAR_budget_email; never commit it."
  type        = string
  sensitive   = true

  validation {
    condition     = can(regex("^[^@[:space:]]+@[^@[:space:]]+\\.[^@[:space:]]+$", var.budget_email))
    error_message = "A valid budget notification email is required before this stack can be applied."
  }
}
