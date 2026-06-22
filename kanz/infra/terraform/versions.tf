# INFRA-01a — provider + Terraform version pins. Cloud foundation is AWS: the
# platform already targets S3 for the DR-01b backups / AUDIT-01b WORM export, so
# the foundation is built on the same provider rather than introducing a second.
# A GCP/Azure port swaps these modules; the layering (network → cluster →
# storage) is provider-agnostic.
terraform {
  required_version = ">= 1.7"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.40"
    }
  }

  # Remote state with locking — never local state for shared infra. Set per
  # environment via `terraform init -backend-config=…` (the bucket/table are
  # bootstrapped out-of-band, the classic chicken-and-egg).
  backend "s3" {}
}

provider "aws" {
  region = var.region
  default_tags {
    tags = {
      "app.kubernetes.io/part-of" = "kanz"
      "kanz.io/managed-by"        = "terraform"
      "kanz.io/environment"       = var.environment
    }
  }
}
