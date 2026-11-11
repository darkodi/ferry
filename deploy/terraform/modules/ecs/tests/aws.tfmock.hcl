# Shapes the AWS mock where Ferry's configuration parses provider values.
mock_data "aws_caller_identity" {
  defaults = { account_id = "123456789012" }
}


mock_resource "aws_secretsmanager_secret" {
  defaults = { arn = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:ferry-dev/mock-AbCdEf" }
}

mock_resource "aws_db_instance" {
  defaults = {
    address = "ferry-dev.mock.eu-west-1.rds.amazonaws.com"
    port    = 5432
    master_user_secret = [{
      secret_arn    = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:rds!db-mock-AbCdEf"
      kms_key_id    = "mock"
      secret_status = "active"
    }]
  }
}

# Resources whose ARNs other resources validate.
mock_resource "aws_iam_role" {
  defaults = { arn = "arn:aws:iam::123456789012:role/mock" }
}

mock_resource "aws_cloudwatch_log_group" {
  defaults = { arn = "arn:aws:logs:eu-west-1:123456789012:log-group:mock" }
}

mock_resource "aws_kms_key" {
  defaults = { arn = "arn:aws:kms:eu-west-1:123456789012:key/00000000-0000-0000-0000-000000000000" }
}

mock_resource "aws_wafv2_web_acl" {
  defaults = { arn = "arn:aws:wafv2:eu-west-1:123456789012:regional/webacl/mock/00000000" }
}

mock_resource "aws_lb" {
  defaults = { arn = "arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/app/mock/0000", arn_suffix = "app/mock/0000" }
}

mock_resource "aws_lb_target_group" {
  defaults = { arn = "arn:aws:elasticloadbalancing:eu-west-1:123456789012:targetgroup/mock/0000", arn_suffix = "targetgroup/mock/0000" }
}

mock_resource "aws_sqs_queue" {
  defaults = { arn = "arn:aws:sqs:eu-west-1:123456789012:mock", url = "https://sqs.eu-west-1.amazonaws.com/123456789012/mock" }
}

mock_resource "aws_sns_topic" {
  defaults = { arn = "arn:aws:sns:eu-west-1:123456789012:mock" }
}

mock_resource "aws_ecr_repository" {
  defaults = { arn = "arn:aws:ecr:eu-west-1:123456789012:repository/mock", repository_url = "123456789012.dkr.ecr.eu-west-1.amazonaws.com/mock" }
}

mock_resource "aws_ecs_cluster" {
  defaults = { arn = "arn:aws:ecs:eu-west-1:123456789012:cluster/mock" }
}

mock_resource "aws_ecs_task_definition" {
  defaults = {
    arn                  = "arn:aws:ecs:eu-west-1:123456789012:task-definition/mock:1"
    arn_without_revision = "arn:aws:ecs:eu-west-1:123456789012:task-definition/mock"
  }
}

mock_resource "aws_s3_bucket" {
  defaults = { arn = "arn:aws:s3:::mock" }
}
