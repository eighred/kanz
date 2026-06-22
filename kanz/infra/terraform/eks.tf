# INFRA-01a — the Kubernetes cluster. Two managed node groups spread across every
# AZ: a general pool for stateless services (api-gateway, risk-engine, audit, …)
# and a tainted stateful pool dedicated to the brokers (Kafka/NATS StatefulSets)
# and CloudNativePG (DR-01b), so their anti-affinity + local-storage PVs land on
# capacity that the stateless autoscaling (INFRA-01c) won't churn.
#
# The data planes themselves (Kafka, NATS, Postgres) are NOT provisioned here —
# they run in-cluster, deployed by GitOps (INFRA-01b) onto this foundation. That
# split keeps Terraform to cloud resources and Argo CD to k8s workloads.
module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 20.8"

  cluster_name    = var.cluster_name
  cluster_version = var.kubernetes_version

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.private_subnets

  cluster_endpoint_public_access = true
  enable_irsa                    = true # IRSA for the DR backup S3 access etc.

  # Core addons; the CSI drivers the workloads mount (SPIFFE, secrets-store) are
  # installed via GitOps, not as managed addons.
  cluster_addons = {
    coredns                = {}
    kube-proxy             = {}
    vpc-cni                = {}
    aws-ebs-csi-driver     = {}
  }

  eks_managed_node_groups = {
    general = {
      instance_types = var.general_instance_types
      min_size       = var.general_desired_size
      max_size       = var.general_desired_size * 4 # headroom for INFRA-01c scale-out
      desired_size   = var.general_desired_size
      subnet_ids     = module.vpc.private_subnets # spread across all AZs
      labels         = { "kanz.io/pool" = "general" }
    }
    stateful = {
      instance_types = var.stateful_instance_types
      min_size       = var.stateful_desired_size
      max_size       = var.stateful_desired_size * 2
      desired_size   = var.stateful_desired_size
      subnet_ids     = module.vpc.private_subnets
      labels         = { "kanz.io/pool" = "stateful" }
      taints = [{
        key    = "kanz.io/stateful"
        value  = "true"
        effect = "NO_SCHEDULE" # only brokers/DB (with the matching toleration) land here
      }]
    }
  }
}
