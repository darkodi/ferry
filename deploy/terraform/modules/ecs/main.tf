# Ferry on ECS Fargate. Every task runs the one signed image (pinned by
# digest), picks its binary with entryPoint, and runs:
#   - non-root, read-only root filesystem, all Linux capabilities dropped;
#   - in private subnets without a public IP;
#   - with its own task role (what the code may do) and execution role
#     (what ECS may do to start it: pull the image, write logs, read only
#     this task's secrets);
#   - beside an ADOT collector that ships traces to X-Ray and the alarmed
#     metrics to CloudWatch.
# kind = "service" runs continuously, "scheduled" runs from EventBridge
# Scheduler (recon), "oneoff" only when started (migrate, by make aws-up).

locals {
  image          = "${aws_ecr_repository.this.repository_url}@${var.image_digest}"
  services       = { for k, t in var.tasks : k => t if t.kind == "service" }
  scheduled      = { for k, t in var.tasks : k => t if t.kind == "scheduled" }
  metrics_ns     = "Ferry"
  metrics_logs   = "/${var.name}/metrics"
  secret_arn     = { for k, t in var.tasks : k => distinct([for v in values(t.secrets) : join(":", slice(split(":", v), 0, 7))]) }
  drop_all_linux = { capabilities = { drop = ["ALL"] } }
}

# ---- registry and cluster ----

resource "aws_ecr_repository" "this" {
  name                 = var.name
  image_tag_mutability = "IMMUTABLE"
  force_delete         = !var.deletion_protection
  image_scanning_configuration {
    scan_on_push = true
  }
  encryption_configuration {
    encryption_type = "KMS"
    kms_key         = var.kms_key_arn
  }
}

resource "aws_ecr_lifecycle_policy" "this" {
  repository = aws_ecr_repository.this.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep the 10 most recent images"
      selection    = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = 10 }
      action       = { type = "expire" }
    }]
  })
}

resource "aws_ecs_cluster" "this" {
  name = var.name
  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

resource "aws_cloudwatch_log_group" "task" {
  for_each          = var.tasks
  name              = "/${var.name}/${each.key}"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.kms_key_arn
}

resource "aws_cloudwatch_log_group" "metrics" {
  name              = local.metrics_logs
  retention_in_days = var.log_retention_days
  kms_key_id        = var.kms_key_arn
}

# ---- permissions ----

resource "aws_iam_role_policy" "execution" {
  for_each = var.tasks
  name     = "start-task"
  role     = var.execution_role_names[each.key]
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid      = "PullImage"
        Effect   = "Allow"
        Action   = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:BatchCheckLayerAvailability"]
        Resource = aws_ecr_repository.this.arn
      },
      {
        # Has no resource-level permissions.
        Sid      = "RegistryLogin"
        Effect   = "Allow"
        Action   = "ecr:GetAuthorizationToken"
        Resource = "*"
      },
      {
        Sid      = "WriteLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.task[each.key].arn}:*"
      },
      ], [for st in [
        {
          Sid      = "ReadThisTasksSecrets"
          Effect   = "Allow"
          Action   = "secretsmanager:GetSecretValue"
          Resource = local.secret_arn[each.key]
        },
        {
          Sid       = "DecryptSecrets"
          Effect    = "Allow"
          Action    = "kms:Decrypt"
          Resource  = var.kms_key_arn
          Condition = { StringEquals = { "kms:ViaService" = "secretsmanager.${var.region}.amazonaws.com" } }
        },
    ] : st if length(local.secret_arn[each.key]) > 0])
  })
}

resource "aws_iam_role_policy" "task" {
  for_each = { for k, t in var.tasks : k => t if t.adot || length(t.permissions) > 0 }
  name     = "task"
  role     = var.task_role_names[each.key]
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat(
      [for i, p in each.value.permissions : {
        Sid      = "Task${i}"
        Effect   = "Allow"
        Action   = p.actions
        Resource = p.resources
      }],
      [for st in [
        {
          # X-Ray has no resource-level permissions for these actions.
          Sid      = "SendTraces"
          Effect   = "Allow"
          Action   = ["xray:PutTraceSegments", "xray:PutTelemetryRecords", "xray:GetSamplingRules", "xray:GetSamplingTargets"]
          Resource = "*"
        },
        {
          Sid      = "SendMetrics"
          Effect   = "Allow"
          Action   = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"]
          Resource = "${aws_cloudwatch_log_group.metrics.arn}:*"
        },
      ] : st if each.value.adot],
    )
  })
}

# ---- task definitions ----

resource "aws_ecs_task_definition" "this" {
  for_each                 = var.tasks
  family                   = "${var.name}-${each.key}"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = each.value.cpu
  memory                   = each.value.memory
  task_role_arn            = var.task_role_arns[each.key]
  execution_role_arn       = var.execution_role_arns[each.key]

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }

  dynamic "volume" {
    for_each = each.value.adot ? ["adot-tmp"] : []
    content {
      name = volume.value
    }
  }

  container_definitions = jsonencode(concat(
    [{
      name                   = each.key
      image                  = local.image
      essential              = true
      entryPoint             = [each.value.entrypoint]
      command                = each.value.command
      user                   = "65532:65532" # distroless nonroot
      readonlyRootFilesystem = true
      linuxParameters        = local.drop_all_linux
      stopTimeout            = 30
      portMappings           = each.value.load_balanced ? [{ containerPort = 8080, protocol = "tcp" }] : []
      environment            = [for k in sort(keys(each.value.environment)) : { name = k, value = each.value.environment[k] }]
      secrets                = [for k in sort(keys(each.value.secrets)) : { name = k, valueFrom = each.value.secrets[k] }]
      dependsOn              = each.value.adot ? [{ containerName = "adot", condition = "START" }] : []
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.task[each.key].name
          awslogs-region        = var.region
          awslogs-stream-prefix = each.key
        }
      }
    }],
    each.value.adot ? [{
      name = "adot"
      # Not essential: a collector crash must not stop payouts; the
      # metrics-missing alarms notice instead.
      essential              = false
      image                  = var.adot_image
      readonlyRootFilesystem = true
      linuxParameters        = local.drop_all_linux
      mountPoints            = [{ sourceVolume = "adot-tmp", containerPath = "/tmp" }]
      environment = [{
        name = "AOT_CONFIG_CONTENT"
        value = templatefile("${path.module}/adot.yaml.tftpl", {
          service      = each.key
          region       = var.region
          namespace    = local.metrics_ns
          log_group    = aws_cloudwatch_log_group.metrics.name
          metrics_port = each.value.metrics_port
        })
      }]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.task[each.key].name
          awslogs-region        = var.region
          awslogs-stream-prefix = "adot"
        }
      }
    }] : [],
  ))
}

# ---- services ----

resource "aws_ecs_service" "this" {
  for_each         = local.services
  name             = each.key
  cluster          = aws_ecs_cluster.this.id
  task_definition  = aws_ecs_task_definition.this[each.key].arn
  desired_count    = var.running ? each.value.desired_count : 0
  launch_type      = "FARGATE"
  platform_version = "LATEST"
  propagate_tags   = "SERVICE"

  # A singleton (a chain watcher) is stopped before its replacement starts,
  # so two never scan the same chain at once.
  deployment_minimum_healthy_percent = each.value.singleton ? 0 : 100
  deployment_maximum_percent         = each.value.singleton ? 100 : 200
  health_check_grace_period_seconds  = each.value.load_balanced ? 30 : null

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  network_configuration {
    subnets          = var.subnet_ids
    security_groups  = [each.value.load_balanced ? var.api_security_group_id : var.tasks_security_group_id]
    assign_public_ip = false
  }

  dynamic "load_balancer" {
    for_each = each.value.load_balanced ? [var.target_group_arn] : []
    content {
      target_group_arn = load_balancer.value
      container_name   = each.key
      container_port   = 8080
    }
  }
}

# ---- scheduled tasks (recon) ----

resource "aws_iam_role" "scheduler" {
  count = length(local.scheduled) > 0 ? 1 : 0
  name  = "${var.name}-scheduler"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "scheduler.amazonaws.com" }
      Action    = "sts:AssumeRole"
      Condition = { StringEquals = { "aws:SourceAccount" = var.account_id } }
    }]
  })
}

resource "aws_iam_role_policy" "scheduler" {
  count = length(local.scheduled) > 0 ? 1 : 0
  name  = "run-scheduled-tasks"
  role  = aws_iam_role.scheduler[0].name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect    = "Allow"
        Action    = "ecs:RunTask"
        Resource  = [for k, _ in local.scheduled : "${aws_ecs_task_definition.this[k].arn_without_revision}:*"]
        Condition = { ArnEquals = { "ecs:cluster" = aws_ecs_cluster.this.arn } }
      },
      {
        Effect   = "Allow"
        Action   = "iam:PassRole"
        Resource = flatten([for k, _ in local.scheduled : [var.task_role_arns[k], var.execution_role_arns[k]]])
      },
    ]
  })
}

resource "aws_scheduler_schedule" "this" {
  for_each                     = local.scheduled
  name                         = "${var.name}-${each.key}"
  schedule_expression          = each.value.schedule
  schedule_expression_timezone = "UTC"
  kms_key_arn                  = var.kms_key_arn
  state                        = var.running ? "ENABLED" : "DISABLED"

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = aws_ecs_cluster.this.arn
    role_arn = aws_iam_role.scheduler[0].arn

    ecs_parameters {
      task_definition_arn = aws_ecs_task_definition.this[each.key].arn_without_revision
      launch_type         = "FARGATE"
      task_count          = 1
      network_configuration {
        subnets          = var.subnet_ids
        security_groups  = [var.tasks_security_group_id]
        assign_public_ip = false
      }
    }

    retry_policy {
      maximum_retry_attempts       = 2
      maximum_event_age_in_seconds = 3600
    }
  }
}
