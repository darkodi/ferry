# Ferry's KMS keys. The three application keys can move money or reveal
# bank details, so their key policies name the exact roles that may use
# them, and the account (administrators, Terraform) may manage them but not
# use them: no Sign or Decrypt through IAM. Giving anyone else that power
# means changing the key policy, which CloudTrail records.
#
#   noah_signing      ECC_NIST_P384, signs Noah API requests (ES384 JWT)
#   solana_authority  ECC_NIST_EDWARDS25519, signs Solana provisioning txs
#   data              symmetric; wraps the key that encrypts bank details
#   infra             symmetric; encrypts logs, RDS, secrets, SNS, ECR. Its
#                     policy delegates to IAM like an AWS-managed key.

locals {
  root = "arn:aws:iam::${var.account_id}:root"

  # Manage, not use. kms:Get* includes GetPublicKey, which reveals nothing.
  # Not kms:CreateGrant: a grant could hand Sign or Decrypt to anyone
  # without touching the key policy.
  admin_actions = [
    "kms:CreateAlias", "kms:Describe*", "kms:Enable*", "kms:List*", "kms:Put*",
    "kms:Update*", "kms:Revoke*", "kms:Disable*", "kms:Get*", "kms:Delete*",
    "kms:TagResource", "kms:UntagResource", "kms:ScheduleKeyDeletion",
    "kms:CancelKeyDeletion", "kms:RotateKeyOnDemand",
  ]
  admin_statement = {
    Sid       = "Administer"
    Effect    = "Allow"
    Principal = { AWS = local.root }
    Action    = local.admin_actions
    Resource  = "*"
  }
}

resource "aws_kms_key" "noah_signing" {
  description              = "${var.name}: Noah request signing (ES384)"
  customer_master_key_spec = "ECC_NIST_P384"
  key_usage                = "SIGN_VERIFY"
  deletion_window_in_days  = var.deletion_window_days
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [local.admin_statement, {
      Sid       = "Sign"
      Effect    = "Allow"
      Principal = { AWS = var.noah_signers }
      Action    = ["kms:Sign", "kms:GetPublicKey", "kms:DescribeKey"]
      Resource  = "*"
    }]
  })
}

resource "aws_kms_key" "solana_authority" {
  description              = "${var.name}: Solana provisioning authority (Ed25519)"
  customer_master_key_spec = "ECC_NIST_EDWARDS25519" # gitleaks:allow (a key spec name, not a secret)
  key_usage                = "SIGN_VERIFY"
  deletion_window_in_days  = var.deletion_window_days
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [local.admin_statement, {
      Sid       = "Sign"
      Effect    = "Allow"
      Principal = { AWS = var.solana_signers }
      Action    = ["kms:Sign", "kms:GetPublicKey", "kms:DescribeKey"]
      Resource  = "*"
      }, {
      Sid       = "ReadPublicKey"
      Effect    = "Allow"
      Principal = { AWS = var.solana_readers }
      Action    = ["kms:GetPublicKey", "kms:DescribeKey"]
      Resource  = "*"
    }]
  })
}

resource "aws_kms_key" "data" {
  description             = "${var.name}: wraps the bank-details data key"
  enable_key_rotation     = true
  deletion_window_in_days = var.deletion_window_days
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [local.admin_statement, {
      Sid       = "Unwrap"
      Effect    = "Allow"
      Principal = { AWS = var.data_key_decrypters }
      Action    = ["kms:Decrypt"]
      Resource  = "*"
      }, {
      # The operator creates the wrapped data key without ever seeing it.
      Sid       = "WrapNewDataKey"
      Effect    = "Allow"
      Principal = { AWS = local.root }
      Action    = ["kms:GenerateDataKeyWithoutPlaintext"]
      Resource  = "*"
    }]
  })
}

resource "aws_kms_key" "infra" {
  description             = "${var.name}: logs, database, secrets, alarms, images"
  enable_key_rotation     = true
  deletion_window_in_days = var.deletion_window_days
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "DelegateToIAM"
      Effect    = "Allow"
      Principal = { AWS = local.root }
      Action    = "kms:*"
      Resource  = "*"
      }, {
      Sid       = "CloudWatchLogs"
      Effect    = "Allow"
      Principal = { Service = "logs.${var.region}.amazonaws.com" }
      Action    = ["kms:Encrypt*", "kms:Decrypt*", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:Describe*"]
      Resource  = "*"
      Condition = {
        ArnLike = { "kms:EncryptionContext:aws:logs:arn" = "arn:aws:logs:${var.region}:${var.account_id}:log-group:*" }
      }
      }, {
      Sid       = "AlarmsPublishToEncryptedTopic"
      Effect    = "Allow"
      Principal = { Service = "cloudwatch.amazonaws.com" }
      Action    = ["kms:Decrypt", "kms:GenerateDataKey*"]
      Resource  = "*"
      Condition = { StringEquals = { "aws:SourceAccount" = var.account_id } }
    }]
  })
}

resource "aws_kms_alias" "noah_signing" {
  name          = "alias/${var.name}-noah-signing"
  target_key_id = aws_kms_key.noah_signing.key_id
}

resource "aws_kms_alias" "solana_authority" {
  name          = "alias/${var.name}-solana-authority"
  target_key_id = aws_kms_key.solana_authority.key_id
}

resource "aws_kms_alias" "data" {
  name          = "alias/${var.name}-data"
  target_key_id = aws_kms_key.data.key_id
}

resource "aws_kms_alias" "infra" {
  name          = "alias/${var.name}-infra"
  target_key_id = aws_kms_key.infra.key_id
}
