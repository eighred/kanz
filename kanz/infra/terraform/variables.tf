# INFRA-01a — inputs. One tfvars file per environment (and per region for the
# DR-01 active/passive pair): `terraform apply -var-file=env/primary.tfvars`.
variable "region" {
  description = "AWS region for this environment (primary or DR)."
  type        = string
}

variable "environment" {
  description = "Environment name (e.g. primary, dr, staging) — tags + state key."
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster name."
  type        = string
  default     = "kanz"
}

variable "kubernetes_version" {
  description = "EKS control-plane version."
  type        = string
  default     = "1.30"
}

variable "vpc_cidr" {
  description = "VPC CIDR. Must not overlap the peer region's CIDR (DR-01a/b cross-region replication peers the two)."
  type        = string
  default     = "10.0.0.0/16"
}

variable "az_count" {
  description = "Number of AZs to spread across. >=3 so the INFRA-01d topology spread + a single-AZ loss (INFRA-01e) still leaves quorum."
  type        = number
  default     = 3
}

# Two node groups: a general pool for stateless services, and a tainted stateful
# pool pinned for the brokers (Kafka/NATS StatefulSets) and CloudNativePG so
# their local-storage PVs and anti-affinity have dedicated capacity.
variable "general_instance_types" {
  type    = list(string)
  default = ["m6i.xlarge"]
}

variable "stateful_instance_types" {
  type    = list(string)
  default = ["m6i.2xlarge"]
}

variable "general_desired_size" {
  type    = number
  default = 3
}

variable "stateful_desired_size" {
  type    = number
  default = 3
}
