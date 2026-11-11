# One task role and one execution role per Ferry task, created empty (trust
# policy only). They exist before the KMS keys so the key policies can name
# them; the ecs module attaches each role's permissions afterwards.
#
#   task role       what the code in the task may do (sign with a KMS key,
#                   use the queue, send traces and metrics)
#   execution role  what ECS may do to start the task (pull the image,
#                   write logs, read the task's secrets)

locals {
  ecs_trust = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
      Action    = "sts:AssumeRole"
      # Only ECS tasks in this account can assume the role (confused deputy).
      Condition = {
        StringEquals = { "aws:SourceAccount" = var.account_id }
        ArnLike      = { "aws:SourceArn" = "arn:aws:ecs:${var.region}:${var.account_id}:*" }
      }
    }]
  })
}

resource "aws_iam_role" "task" {
  for_each           = toset(var.tasks)
  name               = "${var.name}-${each.key}-task"
  assume_role_policy = local.ecs_trust
}

resource "aws_iam_role" "execution" {
  for_each           = toset(var.tasks)
  name               = "${var.name}-${each.key}-exec"
  assume_role_policy = local.ecs_trust
}
