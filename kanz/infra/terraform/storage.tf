# INFRA-01a — durable backup storage. One Object-Lock bucket backs both the
# DR-01b Postgres WAL/base backups and the AUDIT-01b WORM export: COMPLIANCE-mode
# object lock means even an admin cannot delete or overwrite an object before its
# retention elapses — the storage half of "tamper-evident" and "provable RPO".
resource "aws_kms_key" "backups" {
  description             = "kanz ${var.environment} backups encryption"
  enable_key_rotation     = true
  deletion_window_in_days = 30
}

resource "aws_s3_bucket" "backups" {
  bucket              = "kanz-${var.environment}-backups"
  object_lock_enabled = true
}

resource "aws_s3_bucket_versioning" "backups" {
  bucket = aws_s3_bucket.backups.id
  versioning_configuration { status = "Enabled" } # required for object lock
}

resource "aws_s3_bucket_object_lock_configuration" "backups" {
  bucket = aws_s3_bucket.backups.id
  rule {
    default_retention {
      mode = "COMPLIANCE" # not GOVERNANCE — no bypass, even with a privileged role
      days = 35           # >= the DR-01b 30d PITR window so no backup expires early
    }
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "backups" {
  bucket = aws_s3_bucket.backups.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.backups.arn
    }
  }
}

resource "aws_s3_bucket_public_access_block" "backups" {
  bucket                  = aws_s3_bucket.backups.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# IRSA: the CloudNativePG backup ServiceAccount (DR-01b) assumes this role to
# read/write the bucket — no static keys (the kanz-dr-s3 secret becomes IRSA in
# a real deploy). Scoped to this bucket only.
data "aws_iam_policy_document" "backups_rw" {
  statement {
    actions   = ["s3:PutObject", "s3:GetObject", "s3:ListBucket", "s3:DeleteObject"]
    resources = [aws_s3_bucket.backups.arn, "${aws_s3_bucket.backups.arn}/*"]
  }
  statement {
    actions   = ["kms:Encrypt", "kms:Decrypt", "kms:GenerateDataKey"]
    resources = [aws_kms_key.backups.arn]
  }
}

module "backups_irsa" {
  source  = "terraform-aws-modules/iam/aws//modules/iam-role-for-service-accounts-eks"
  version = "~> 5.39"

  role_name = "kanz-${var.environment}-backups"
  role_policy_arns = { inline = aws_iam_policy.backups_rw.arn }
  oidc_providers = {
    main = {
      provider_arn               = module.eks.oidc_provider_arn
      namespace_service_accounts = ["kanz-data:kanz-risk", "kanz-data:kanz-registry"]
    }
  }
}

resource "aws_iam_policy" "backups_rw" {
  name   = "kanz-${var.environment}-backups-rw"
  policy = data.aws_iam_policy_document.backups_rw.json
}
