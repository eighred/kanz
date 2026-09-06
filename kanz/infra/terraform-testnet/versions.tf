terraform {
  required_version = "~> 1.15.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      "app.kubernetes.io/part-of" = "kanz"
      "kanz.io/environment"       = "testnet"
      "kanz.io/managed-by"        = "terraform"
      "kanz.io/issue"             = "71"
    }
  }
}
