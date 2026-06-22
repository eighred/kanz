# INFRA-01a — network. Multi-AZ VPC (the substrate for the INFRA-01d topology
# spread and the INFRA-01e AZ-kill test): private subnets for the workloads,
# public subnets for the ingress/NAT. Subnets are tagged for EKS + Karpenter/LB
# discovery.
locals {
  azs = slice(data.aws_availability_zones.available.names, 0, var.az_count)
}

data "aws_availability_zones" "available" {
  state = "available"
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 5.8"

  name = "${var.cluster_name}-${var.environment}"
  cidr = var.vpc_cidr
  azs  = local.azs

  # /19 private (workloads) + /24 public (ingress/NAT), one per AZ.
  private_subnets = [for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 3, i)]
  public_subnets  = [for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 8, i + 48)]

  enable_nat_gateway   = true
  single_nat_gateway   = false # one NAT per AZ — a NAT's AZ loss must not strand the others
  one_nat_gateway_per_az = true
  enable_dns_hostnames = true

  # EKS + AWS LB controller subnet discovery.
  private_subnet_tags = {
    "kubernetes.io/role/internal-elb"             = "1"
    "kubernetes.io/cluster/${var.cluster_name}"   = "shared"
  }
  public_subnet_tags = {
    "kubernetes.io/role/elb"                      = "1"
    "kubernetes.io/cluster/${var.cluster_name}"   = "shared"
  }
}
