data "aws_availability_zones" "available" {
  state = "available"
}

data "aws_ssm_parameter" "al2023_x86_64" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
}

data "aws_caller_identity" "current" {}

locals {
  availability_zone = data.aws_availability_zones.available.names[0]
  capital_path_images = toset([
    "accounting",
    "api-gateway",
    "audit",
    "compliance",
    "identity",
    "kanz-capitalpath",
    "kanz-migrate",
    "oms",
    "risk-engine",
    "venue-binance",
    "venue-okx",
  ])
  common_tags = {
    Name                   = var.name
    "kanz.io/capital-path" = "testnet-only"
    "kanz.io/owner"        = "platform"
  }
}

resource "aws_ecr_repository" "capital_path" {
  for_each = local.capital_path_images

  name                 = each.value
  image_tag_mutability = "IMMUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }

  tags = merge(local.common_tags, {
    Name                  = each.value
    "kanz.io/image-scope" = "capital-path"
  })
}

resource "aws_ecr_lifecycle_policy" "capital_path" {
  for_each   = aws_ecr_repository.capital_path
  repository = each.value.name
  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Retain the 20 newest immutable commit images"
        selection = {
          tagStatus      = "tagged"
          tagPatternList = ["*"]
          countType      = "imageCountMoreThan"
          countNumber    = 20
        }
        action = { type = "expire" }
      },
      {
        rulePriority = 2
        description  = "Remove abandoned untagged layers after seven days"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 7
        }
        action = { type = "expire" }
      },
    ]
  })
}

resource "aws_iam_openid_connect_provider" "github_actions" {
  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]
  tags           = local.common_tags
}

resource "aws_iam_role" "github_ecr_publish" {
  name                 = "${var.name}-github-ecr-publish"
  max_session_duration = 3600
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Federated = aws_iam_openid_connect_provider.github_actions.arn
      }
      Action = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "token.actions.githubusercontent.com:aud" = "sts.amazonaws.com"
          "token.actions.githubusercontent.com:sub" = "repo:eighred/kanz:ref:refs/heads/main"
        }
      }
    }]
  })
  tags = local.common_tags
}

resource "aws_iam_role_policy" "github_ecr_publish" {
  name = "${var.name}-ecr-publish"
  role = aws_iam_role.github_ecr_publish.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "RegistryLogin"
        Effect   = "Allow"
        Action   = "ecr:GetAuthorizationToken"
        Resource = "*"
      },
      {
        Sid    = "PublishCapitalPathImages"
        Effect = "Allow"
        Action = [
          "ecr:BatchCheckLayerAvailability",
          "ecr:CompleteLayerUpload",
          "ecr:InitiateLayerUpload",
          "ecr:PutImage",
          "ecr:UploadLayerPart",
        ]
        Resource = [for repository in aws_ecr_repository.capital_path : repository.arn]
      },
    ]
  })
}

resource "aws_iam_role_policy" "node_ecr_pull" {
  name = "${var.name}-ecr-pull"
  role = aws_iam_role.node.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "RegistryLogin"
        Effect   = "Allow"
        Action   = "ecr:GetAuthorizationToken"
        Resource = "*"
      },
      {
        Sid    = "PullCapitalPathImages"
        Effect = "Allow"
        Action = [
          "ecr:BatchGetImage",
          "ecr:GetDownloadUrlForLayer",
        ]
        Resource = [for repository in aws_ecr_repository.capital_path : repository.arn]
      },
    ]
  })
}

resource "aws_budgets_budget" "monthly" {
  name         = "${var.name}-monthly"
  budget_type  = "COST"
  limit_amount = "100"
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  cost_types {
    include_credit             = false
    include_discount           = true
    include_other_subscription = true
    include_recurring          = true
    include_refund             = false
    include_subscription       = true
    include_support            = true
    include_tax                = true
    include_upfront            = true
    use_amortized              = false
    use_blended                = false
  }

  dynamic "notification" {
    for_each = toset(["50", "75", "90", "100"])
    content {
      comparison_operator        = "GREATER_THAN"
      threshold                  = notification.value
      threshold_type             = "ABSOLUTE_VALUE"
      notification_type          = "ACTUAL"
      subscriber_email_addresses = [var.budget_email]
    }
  }
}

resource "aws_vpc" "testnet" {
  cidr_block           = "10.71.0.0/24"
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = local.common_tags
}

resource "aws_internet_gateway" "testnet" {
  vpc_id = aws_vpc.testnet.id
  tags   = local.common_tags
}

resource "aws_subnet" "testnet" {
  vpc_id                  = aws_vpc.testnet.id
  availability_zone       = local.availability_zone
  cidr_block              = "10.71.0.0/26"
  map_public_ip_on_launch = false
  tags                    = local.common_tags
}

resource "aws_route_table" "testnet" {
  vpc_id = aws_vpc.testnet.id
  tags   = local.common_tags
}

resource "aws_route" "internet" {
  route_table_id         = aws_route_table.testnet.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.testnet.id
}

resource "aws_route_table_association" "testnet" {
  subnet_id      = aws_subnet.testnet.id
  route_table_id = aws_route_table.testnet.id
}

resource "aws_security_group" "node" {
  name        = "${var.name}-node"
  description = "No ingress; SSM administration and outbound testnet connectivity only"
  vpc_id      = aws_vpc.testnet.id

  tags = local.common_tags

  lifecycle {
    create_before_destroy = true

    postcondition {
      condition     = length(self.ingress) == 0
      error_message = "The testnet node must not expose any inbound security-group rule."
    }
  }
}

resource "aws_vpc_security_group_egress_rule" "https" {
  security_group_id = aws_security_group.node.id
  description       = "TLS to AWS control planes, registries, and exchange testnets"
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 443
  to_port           = 443
  ip_protocol       = "tcp"
}

resource "aws_vpc_security_group_egress_rule" "dns_udp" {
  security_group_id = aws_security_group.node.id
  description       = "DNS resolution"
  cidr_ipv4         = "10.71.0.2/32"
  from_port         = 53
  to_port           = 53
  ip_protocol       = "udp"
}

resource "aws_vpc_security_group_egress_rule" "dns_tcp" {
  security_group_id = aws_security_group.node.id
  description       = "DNS TCP fallback"
  cidr_ipv4         = "10.71.0.2/32"
  from_port         = 53
  to_port           = 53
  ip_protocol       = "tcp"
}

resource "aws_vpc_security_group_egress_rule" "ntp" {
  security_group_id = aws_security_group.node.id
  description       = "Clock synchronization"
  cidr_ipv4         = "169.254.169.123/32"
  from_port         = 123
  to_port           = 123
  ip_protocol       = "udp"
}

resource "aws_iam_role" "node" {
  name = "${var.name}-node"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ec2.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })
  tags = local.common_tags
}

resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_instance_profile" "node" {
  name = "${var.name}-node"
  role = aws_iam_role.node.name
  tags = local.common_tags
}

resource "aws_kms_key" "vault_unseal" {
  description             = "Vault auto-unseal for the Issue 71 Tokyo testnet"
  deletion_window_in_days = 30
  enable_key_rotation     = true
  key_usage               = "ENCRYPT_DECRYPT"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "AccountRootAdministration"
        Effect    = "Allow"
        Principal = { AWS = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:root" }
        Action    = "kms:*"
        Resource  = "*"
      },
      {
        Sid       = "VaultAutoUnsealOnly"
        Effect    = "Allow"
        Principal = { AWS = aws_iam_role.node.arn }
        Action    = ["kms:Encrypt", "kms:Decrypt", "kms:DescribeKey"]
        Resource  = "*"
      }
    ]
  })

  tags = local.common_tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_kms_alias" "vault_unseal" {
  name          = "alias/kanz-vault-unseal"
  target_key_id = aws_kms_key.vault_unseal.key_id
}

resource "aws_iam_role_policy" "vault_unseal" {
  name = "${var.name}-vault-unseal"
  role = aws_iam_role.node.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["kms:Encrypt", "kms:Decrypt", "kms:DescribeKey"]
      Resource = aws_kms_key.vault_unseal.arn
    }]
  })
}

# Issue #1100: the single-node Tokyo estate has no recovery boundary if its
# attached EBS volume or Region is lost.  This bucket is in Osaka and holds only
# encrypted, redacted recovery artifacts.  Governance-mode Object Lock protects
# a drill from an accidental or ordinary-operator delete while retaining a
# controlled escape hatch during testnet development; production evidence uses
# the separately governed compliance/air-gapped posture.
resource "aws_kms_key" "recovery" {
  provider                = aws.recovery
  description             = "Kanz Tokyo testnet recovery artifacts in Osaka"
  deletion_window_in_days = 30
  enable_key_rotation     = true
  key_usage               = "ENCRYPT_DECRYPT"

  tags = merge(local.common_tags, {
    Name = "${var.name}-recovery"
  })

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_kms_alias" "recovery" {
  provider      = aws.recovery
  name          = "alias/${var.name}-recovery"
  target_key_id = aws_kms_key.recovery.key_id
}

resource "aws_s3_bucket" "recovery" {
  provider            = aws.recovery
  bucket              = "${var.name}-recovery-${data.aws_caller_identity.current.account_id}"
  object_lock_enabled = true

  tags = merge(local.common_tags, {
    Name                     = "${var.name}-recovery"
    "kanz.io/data-residency" = "jp"
  })

}

resource "aws_s3_bucket_versioning" "recovery" {
  provider = aws.recovery
  bucket   = aws_s3_bucket.recovery.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_object_lock_configuration" "recovery" {
  provider = aws.recovery
  bucket   = aws_s3_bucket.recovery.id

  rule {
    default_retention {
      mode = "GOVERNANCE"
      days = 30
    }
  }

  depends_on = [aws_s3_bucket_versioning.recovery]
}

# Object Lock prevents deletion during the evidence window; lifecycle bounds
# storage after that window.  Noncurrent versions get the same bound so a
# replaced drill bundle cannot accumulate forever behind versioning.
resource "aws_s3_bucket_lifecycle_configuration" "recovery" {
  provider = aws.recovery
  bucket   = aws_s3_bucket.recovery.id

  rule {
    id     = "expire-recovery-evidence"
    status = "Enabled"

    filter {}

    expiration {
      days = 45
    }

    noncurrent_version_expiration {
      noncurrent_days = 45
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }

  depends_on = [aws_s3_bucket_object_lock_configuration.recovery]
}

resource "aws_s3_bucket_public_access_block" "recovery" {
  provider = aws.recovery
  bucket   = aws_s3_bucket.recovery.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "recovery" {
  provider = aws.recovery
  bucket   = aws_s3_bucket.recovery.id

  rule {
    bucket_key_enabled = true
    apply_server_side_encryption_by_default {
      kms_master_key_id = aws_kms_key.recovery.arn
      sse_algorithm     = "aws:kms"
    }
  }
}

resource "aws_s3_bucket_policy" "recovery" {
  provider = aws.recovery
  bucket   = aws_s3_bucket.recovery.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "DenyInsecureTransport"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:*"
        Resource = [
          aws_s3_bucket.recovery.arn,
          "${aws_s3_bucket.recovery.arn}/*",
        ]
        Condition = {
          Bool = { "aws:SecureTransport" = "false" }
        }
      },
      {
        Sid       = "DenyUnapprovedEncryption"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.recovery.arn}/*"
        Condition = {
          StringNotEquals = {
            "s3:x-amz-server-side-encryption"                = "aws:kms"
            "s3:x-amz-server-side-encryption-aws-kms-key-id" = aws_kms_key.recovery.arn
          }
        }
      },
    ]
  })
}

# Only the SSM-controlled host can write or read recovery objects. IMDSv2's hop
# limit remains one, so ordinary pods cannot inherit this role. Backup tooling
# runs on the host and never places AWS credentials in a Pod or Vault.
resource "aws_iam_role_policy" "node_recovery" {
  name = "${var.name}-recovery"
  role = aws_iam_role.node.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "ListRecoveryBucket"
        Effect   = "Allow"
        Action   = ["s3:GetBucketLocation", "s3:ListBucket"]
        Resource = aws_s3_bucket.recovery.arn
      },
      {
        Sid      = "ReadWriteRecoveryObjects"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:GetObjectVersion", "s3:GetObjectRetention", "s3:PutObject"]
        Resource = "${aws_s3_bucket.recovery.arn}/*"
      },
      {
        Sid      = "UseRecoveryKey"
        Effect   = "Allow"
        Action   = ["kms:Decrypt", "kms:Encrypt", "kms:GenerateDataKey"]
        Resource = aws_kms_key.recovery.arn
      },
    ]
  })
}

resource "aws_ebs_volume" "data" {
  availability_zone = local.availability_zone
  encrypted         = true
  size              = var.data_volume_gib
  type              = "gp3"
  iops              = 3000
  throughput        = 125

  tags = merge(local.common_tags, {
    Name             = "${var.name}-data"
    "kanz.io/backup" = "true"
  })

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_instance" "node" {
  ami                         = data.aws_ssm_parameter.al2023_x86_64.value
  instance_type               = var.instance_type
  availability_zone           = local.availability_zone
  subnet_id                   = aws_subnet.testnet.id
  vpc_security_group_ids      = [aws_security_group.node.id]
  iam_instance_profile        = aws_iam_instance_profile.node.name
  associate_public_ip_address = false
  monitoring                  = false
  disable_api_termination     = true
  ebs_optimized               = true

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "disabled"
  }

  credit_specification {
    cpu_credits = "standard"
  }

  root_block_device {
    encrypted             = true
    volume_type           = "gp3"
    volume_size           = var.root_volume_gib
    iops                  = 3000
    throughput            = 125
    delete_on_termination = true
  }

  user_data = templatefile("${path.module}/user_data.sh.tftpl", {
    data_volume_id        = aws_ebs_volume.data.id
    ecr_provider_revision = "7656c21bcc13700566830f6bc4d753063513e6f7"
    k3s_version           = "v1.36.4+k3s1"
    k3s_install_sha       = "46177d4c99440b4c0311b67233823a8e8a2fc09693f6c89af1a7161e152fbfad"
    k3s_install_commit    = "4dedb15be78017a8ddd5b9e81acd44f3481078ed"
  })

  tags = merge(local.common_tags, {
    Name = "${var.name}-node"
  })

  depends_on = [
    aws_iam_role_policy_attachment.ssm,
    aws_route.internet,
  ]

  lifecycle {
    # The EIP and durable data disk are owned by their dedicated resources.
    # EC2 reflects both back onto the instance, which otherwise creates false
    # replacement drift in the AWS provider.
    ignore_changes = [
      associate_public_ip_address,
      ebs_block_device,
    ]

    precondition {
      condition     = var.region == "ap-northeast-1" && var.instance_type == "t3a.large"
      error_message = "The reviewed Issue #71 envelope is Tokyo t3a.large only."
    }
  }
}

resource "aws_volume_attachment" "data" {
  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.data.id
  instance_id = aws_instance.node.id
}

resource "aws_eip" "node" {
  domain   = "vpc"
  instance = aws_instance.node.id
  tags     = local.common_tags
}

resource "aws_iam_role" "dlm" {
  name = "${var.name}-dlm"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "dlm.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })
  tags = local.common_tags
}

resource "aws_iam_role_policy" "dlm" {
  name = "${var.name}-snapshots"
  role = aws_iam_role.dlm.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "ec2:CreateSnapshot",
          "ec2:CreateSnapshots",
          "ec2:DeleteSnapshot",
          "ec2:DescribeInstances",
          "ec2:DescribeSnapshots",
          "ec2:DescribeTags",
          "ec2:DescribeVolumes"
        ]
        Resource = "*"
      },
      {
        Effect   = "Allow"
        Action   = ["ec2:CreateTags"]
        Resource = "arn:aws:ec2:${var.region}::snapshot/*"
      }
    ]
  })
}

resource "aws_dlm_lifecycle_policy" "data" {
  description        = "Daily snapshots for the Issue 71 Tokyo testnet data volume"
  execution_role_arn = aws_iam_role.dlm.arn
  state              = "ENABLED"

  policy_details {
    resource_types = ["VOLUME"]
    target_tags = {
      "kanz.io/backup" = "true"
    }

    schedule {
      name      = "daily-seven-day-retention"
      copy_tags = true

      create_rule {
        interval      = 24
        interval_unit = "HOURS"
        times         = ["18:00"]
      }

      retain_rule {
        count = 7
      }

      tags_to_add = {
        "kanz.io/restore-class" = "testnet-dr-evidence"
      }
    }
  }

  tags       = local.common_tags
  depends_on = [aws_iam_role_policy.dlm]
}
