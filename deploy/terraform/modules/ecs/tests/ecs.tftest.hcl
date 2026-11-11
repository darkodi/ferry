# Container hardening, digest pinning and per-task secret scoping.
# Run: make tf-test
mock_provider "aws" {
  source = "./tests"
}

variables {
  name         = "ferry-test"
  region       = "eu-west-1"
  account_id   = "123456789012"
  image_digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
  adot_image   = "public.ecr.aws/aws-observability/aws-otel-collector:v0.50.0@sha256:7968fb60db6a2390a47ba6a2df029745638486e285c9b2487da1b722d0855a3e"
  running      = true
  tasks = {
    api = {
      kind          = "service"
      entrypoint    = "/ferry/api"
      load_balanced = true
      metrics_port  = 9100
      secrets       = { FERRY_ADMIN_KEY = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:t/admin-key-AbCdEf" }
    }
    watcher = {
      kind       = "service"
      entrypoint = "/ferry/watcher"
      singleton  = true
      secrets    = { PGPASSWORD = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:t/db-AbCdEf" }
    }
    recon = {
      kind       = "scheduled"
      entrypoint = "/ferry/recon"
      schedule   = "cron(15 3 * * ? *)"
    }
    migrate = {
      kind       = "oneoff"
      entrypoint = "/ferry/migrate"
      adot       = false
      secrets    = { PGPASSWORD = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:rds!db-x-AbCdEf:password::" }
    }
  }
  task_role_arns          = { api = "arn:aws:iam::123456789012:role/a", watcher = "arn:aws:iam::123456789012:role/w", recon = "arn:aws:iam::123456789012:role/r", migrate = "arn:aws:iam::123456789012:role/m" }
  task_role_names         = { api = "a", watcher = "w", recon = "r", migrate = "m" }
  execution_role_arns     = { api = "arn:aws:iam::123456789012:role/ae", watcher = "arn:aws:iam::123456789012:role/we", recon = "arn:aws:iam::123456789012:role/re", migrate = "arn:aws:iam::123456789012:role/me" }
  execution_role_names    = { api = "ae", watcher = "we", recon = "re", migrate = "me" }
  subnet_ids              = ["subnet-1", "subnet-2"]
  api_security_group_id   = "sg-api"
  tasks_security_group_id = "sg-tasks"
  target_group_arn        = "arn:aws:elasticloadbalancing:eu-west-1:123456789012:targetgroup/t/0"
  kms_key_arn             = "arn:aws:kms:eu-west-1:123456789012:key/infra"
  log_retention_days      = 30
  deletion_protection     = false
}

run "hardening" {
  command = apply

  assert {
    condition = alltrue(flatten([for td in aws_ecs_task_definition.this : [for c in jsondecode(td.container_definitions) :
    c.readonlyRootFilesystem && c.linuxParameters.capabilities.drop == ["ALL"]]]))
    error_message = "Every container needs a read-only root filesystem and no Linux capabilities."
  }
  assert {
    condition     = alltrue([for td in aws_ecs_task_definition.this : [for c in jsondecode(td.container_definitions) : c if c.name != "adot"][0].user == "65532:65532"])
    error_message = "Ferry containers run as the distroless nonroot user."
  }
  assert {
    condition = alltrue(flatten([for td in aws_ecs_task_definition.this : [for c in jsondecode(td.container_definitions) :
    can(regex("@sha256:[0-9a-f]{64}$", c.image))]]))
    error_message = "Every image is pinned by digest."
  }
  assert {
    condition     = toset([for k, s in aws_ecs_service.this : k if length(s.load_balancer) > 0]) == toset(["api"])
    error_message = "Only the load-balanced task registers with the target group."
  }
  assert {
    condition     = aws_ecs_service.this["watcher"].deployment_maximum_percent == 100 && aws_ecs_service.this["watcher"].deployment_minimum_healthy_percent == 0
    error_message = "A singleton must never run twice during a deploy."
  }
  assert {
    condition     = !contains(keys(aws_ecs_service.this), "recon") && !contains(keys(aws_ecs_service.this), "migrate")
    error_message = "Scheduled and one-off tasks are not services."
  }
  assert {
    condition     = toset(flatten([for st in jsondecode(aws_iam_role_policy.execution["migrate"].policy).Statement : st.Resource if st.Action == "secretsmanager:GetSecretValue"])) == toset(["arn:aws:secretsmanager:eu-west-1:123456789012:secret:rds!db-x-AbCdEf"])
    error_message = "An execution role reads only its own task's secrets (JSON key suffix stripped)."
  }
  assert {
    condition     = length([for st in jsondecode(aws_iam_role_policy.execution["recon"].policy).Statement : st if st.Action == "secretsmanager:GetSecretValue"]) == 0
    error_message = "A task without secrets gets no secret access."
  }
}

run "stopped" {
  command = apply
  variables {
    running = false
  }
  assert {
    condition     = alltrue([for s in aws_ecs_service.this : s.desired_count == 0]) && aws_scheduler_schedule.this["recon"].state == "DISABLED"
    error_message = "running = false must stop the services and the schedule."
  }
}

run "tag_refused" {
  command = plan
  variables {
    image_digest = "latest"
  }
  expect_failures = [var.image_digest]
}
