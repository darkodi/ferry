# RDS Postgres 16 in the private subnets. Encrypted at rest with the infra
# key; TLS is required (rds.force_ssl) and the services verify the server
# certificate against the RDS CA bundle shipped in the image. The master
# password is generated and kept by RDS in Secrets Manager: it never
# appears in Terraform state, and only the migrate task can read it. The
# services connect as a separate login role that is a member of ferry_app
# (created by `migrate -app-login`).

resource "aws_db_subnet_group" "this" {
  name       = var.name
  subnet_ids = var.subnet_ids
}

resource "aws_db_parameter_group" "this" {
  name   = "${var.name}-pg16"
  family = "postgres16"

  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }
  parameter {
    name  = "log_connections"
    value = "1"
  }
  parameter {
    name  = "log_disconnections"
    value = "1"
  }
  # Log failed statements (constraint violations, permission denials) but
  # never statement text by default: it could hold the app login's password.
  parameter {
    name  = "log_min_error_statement"
    value = "error"
  }
}

# Enhanced monitoring: OS-level metrics every 60 s.
resource "aws_iam_role" "monitoring" {
  name = "${var.name}-rds-monitoring"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "monitoring.rds.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "monitoring" {
  role       = aws_iam_role.monitoring.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonRDSEnhancedMonitoringRole"
}

resource "aws_db_instance" "this" {
  #checkov:skip=CKV_AWS_161:Services log in with a password from Secrets Manager; IAM auth tokens expire every 15 minutes and need a pgx BeforeConnect hook (recorded as later work in the threat model).
  #checkov:skip=CKV_AWS_157:Multi-AZ is var.multi_az, off in dev for cost; turn it on for production.
  #checkov:skip=CKV_AWS_293:Deletion protection is var.deletion_protection, off in dev so make aws-down can remove a demo; on for production.
  identifier     = var.name
  engine         = "postgres"
  engine_version = "16"
  instance_class = var.instance_class

  db_name                       = "ferry"
  username                      = "ferry_owner"
  manage_master_user_password   = true
  master_user_secret_kms_key_id = var.kms_key_arn

  allocated_storage     = 20
  max_allocated_storage = 50
  storage_type          = "gp3"
  storage_encrypted     = true
  kms_key_id            = var.kms_key_arn

  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = [var.security_group_id]
  publicly_accessible    = false
  multi_az               = var.multi_az
  parameter_group_name   = aws_db_parameter_group.this.name
  ca_cert_identifier     = "rds-ca-rsa2048-g1"

  auto_minor_version_upgrade      = true
  backup_retention_period         = 7
  copy_tags_to_snapshot           = true
  deletion_protection             = var.deletion_protection
  skip_final_snapshot             = !var.deletion_protection
  final_snapshot_identifier       = var.deletion_protection ? "${var.name}-final" : null
  enabled_cloudwatch_logs_exports = ["postgresql", "upgrade"]

  monitoring_interval = 60
  monitoring_role_arn = aws_iam_role.monitoring.arn

  performance_insights_enabled    = true
  performance_insights_kms_key_id = var.kms_key_arn
}
