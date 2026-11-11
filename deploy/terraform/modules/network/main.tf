# VPC across two availability zones. Public subnets hold only the load
# balancer and the NAT gateway; tasks and the database live in private
# subnets with no public IPs. One NAT gateway (not one per AZ) keeps the dev
# bill down at the cost of AZ redundancy for outbound calls. VPC endpoints
# keep AWS API traffic (ECR, logs, KMS, secrets, SQS, X-Ray) off the NAT and
# inside AWS; the NAT carries only Noah and chain RPC calls.

locals {
  # A fixed pair of zones: the plan never changes because AWS added one.
  azs = coalesce(var.availability_zones, ["${var.region}a", "${var.region}b"])
  interface_endpoints = var.interface_endpoints ? toset([
    "ecr.api", "ecr.dkr", "logs", "kms", "secretsmanager", "sqs", "xray",
  ]) : toset([])
}

resource "aws_vpc" "this" {
  cidr_block           = var.cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = var.name }
}

# The default security group allows nothing; every resource gets its own.
resource "aws_default_security_group" "this" {
  vpc_id = aws_vpc.this.id
}

resource "aws_subnet" "public" {
  count             = 2
  vpc_id            = aws_vpc.this.id
  cidr_block        = cidrsubnet(var.cidr, 8, count.index)
  availability_zone = local.azs[count.index]
  tags              = { Name = "${var.name}-public-${count.index}" }
}

resource "aws_subnet" "private" {
  count             = 2
  vpc_id            = aws_vpc.this.id
  cidr_block        = cidrsubnet(var.cidr, 8, 10 + count.index)
  availability_zone = local.azs[count.index]
  tags              = { Name = "${var.name}-private-${count.index}" }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
}

resource "aws_eip" "nat" {
  domain = "vpc"
}

resource "aws_nat_gateway" "this" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public[0].id
  depends_on    = [aws_internet_gateway.this]
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.this.id
  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.this.id
  }
}

resource "aws_route_table_association" "public" {
  count          = 2
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table_association" "private" {
  count          = 2
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}

# ---- flow logs: every accepted and rejected connection, for forensics ----

resource "aws_cloudwatch_log_group" "flow" {
  name              = "/${var.name}/vpc-flow"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.kms_key_arn
}

resource "aws_iam_role" "flow" {
  name = "${var.name}-vpc-flow"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "vpc-flow-logs.amazonaws.com" }
      Action    = "sts:AssumeRole"
      Condition = { StringEquals = { "aws:SourceAccount" = var.account_id } }
    }]
  })
}

resource "aws_iam_role_policy" "flow" {
  role = aws_iam_role.flow.name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"]
      Resource = "${aws_cloudwatch_log_group.flow.arn}:*"
    }]
  })
}

resource "aws_flow_log" "this" {
  vpc_id          = aws_vpc.this.id
  traffic_type    = "ALL"
  log_destination = aws_cloudwatch_log_group.flow.arn
  iam_role_arn    = aws_iam_role.flow.arn
}

# ---- security groups ----
# alb   443/80 from the internet
# api   8080 from the alb only
# tasks no inbound (relay, worker, watchers, recon, migrate)
# db    5432 from api and tasks only
# Outbound from api and tasks: 443 (Noah, chain RPC, AWS endpoints) and the
# database; nothing else.

resource "aws_security_group" "alb" {
  #checkov:skip=CKV2_AWS_5:Attached in another module (edge, ecs, data); checkov does not follow module outputs.
  name        = "${var.name}-alb"
  description = "Load balancer: HTTPS from the internet, HTTP only to redirect"
  vpc_id      = aws_vpc.this.id
}

resource "aws_security_group" "api" {
  #checkov:skip=CKV2_AWS_5:Attached in another module (edge, ecs, data); checkov does not follow module outputs.
  name        = "${var.name}-api"
  description = "API tasks: traffic from the load balancer only"
  vpc_id      = aws_vpc.this.id
}

resource "aws_security_group" "tasks" {
  #checkov:skip=CKV2_AWS_5:Attached in another module (edge, ecs, data); checkov does not follow module outputs.
  name        = "${var.name}-tasks"
  description = "Background tasks: no inbound traffic"
  vpc_id      = aws_vpc.this.id
}

resource "aws_security_group" "db" {
  #checkov:skip=CKV2_AWS_5:Attached in another module (edge, ecs, data); checkov does not follow module outputs.
  name        = "${var.name}-db"
  description = "Postgres: from Ferry tasks only"
  vpc_id      = aws_vpc.this.id
}

resource "aws_security_group" "endpoints" {
  name        = "${var.name}-endpoints"
  description = "VPC interface endpoints: HTTPS from inside the VPC"
  vpc_id      = aws_vpc.this.id
}

resource "aws_vpc_security_group_ingress_rule" "alb_https" {
  security_group_id = aws_security_group.alb.id
  description       = "HTTPS from the internet"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}

resource "aws_vpc_security_group_ingress_rule" "alb_http" {
  #checkov:skip=CKV_AWS_260:Port 80 only answers with a redirect to HTTPS (aws_lb_listener.http); no content is served over HTTP.
  security_group_id = aws_security_group.alb.id
  description       = "HTTP from the internet, answered with a redirect to HTTPS"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 80
  to_port           = 80
}

resource "aws_vpc_security_group_egress_rule" "alb_to_api" {
  security_group_id            = aws_security_group.alb.id
  description                  = "To the API tasks"
  referenced_security_group_id = aws_security_group.api.id
  ip_protocol                  = "tcp"
  from_port                    = 8080
  to_port                      = 8080
}

resource "aws_vpc_security_group_ingress_rule" "api_from_alb" {
  security_group_id            = aws_security_group.api.id
  description                  = "From the load balancer"
  referenced_security_group_id = aws_security_group.alb.id
  ip_protocol                  = "tcp"
  from_port                    = 8080
  to_port                      = 8080
}

resource "aws_vpc_security_group_egress_rule" "https_out" {
  for_each          = { api = aws_security_group.api.id, tasks = aws_security_group.tasks.id }
  security_group_id = each.value
  description       = "HTTPS to Noah, chain RPC and AWS endpoints"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}

resource "aws_vpc_security_group_egress_rule" "db_out" {
  for_each                     = { api = aws_security_group.api.id, tasks = aws_security_group.tasks.id }
  security_group_id            = each.value
  description                  = "To Postgres"
  referenced_security_group_id = aws_security_group.db.id
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
}

resource "aws_vpc_security_group_ingress_rule" "db_in" {
  for_each                     = { api = aws_security_group.api.id, tasks = aws_security_group.tasks.id }
  security_group_id            = aws_security_group.db.id
  description                  = "From Ferry ${each.key}"
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
}

resource "aws_vpc_security_group_ingress_rule" "endpoints_in" {
  security_group_id = aws_security_group.endpoints.id
  description       = "HTTPS from inside the VPC"
  cidr_ipv4         = var.cidr
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}

# ---- VPC endpoints ----

resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.private.id] # ECR image layers come from S3
}

resource "aws_vpc_endpoint" "interface" {
  for_each            = local.interface_endpoints
  vpc_id              = aws_vpc.this.id
  service_name        = "com.amazonaws.${var.region}.${each.key}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = aws_subnet.private[*].id
  security_group_ids  = [aws_security_group.endpoints.id]
  private_dns_enabled = true
}
