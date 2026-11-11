# CloudWatch alarms that mirror the critical rules in
# deploy/prometheus/alerts.yml, notifying an encrypted SNS topic with an
# email subscription, plus a monthly AWS Budgets alert. Ferry's metrics
# arrive from the ADOT sidecars (namespace "Ferry", dimension "service");
# queue and load balancer alarms use AWS's own metrics.
#
# Missing data: an alarm on a gauge the API always reports (outbox lag)
# treats missing data as breaching, so "the API stopped reporting" also
# pages. Counters only exist once incremented, so missing means zero.

resource "aws_sns_topic" "alarms" {
  name              = "${var.name}-alarms"
  kms_master_key_id = var.kms_key_arn
}

resource "aws_sns_topic_subscription" "email" {
  topic_arn = aws_sns_topic.alarms.arn
  protocol  = "email"
  endpoint  = var.email
}

locals {
  ferry = {
    invariant-violation = {
      description = "Internal reconciliation found a broken ledger invariant. Stop payouts and investigate (see FerryInvariantViolation)."
      metric      = "ferry_recon_breaks_open"
      dimensions  = { service = "api", check = "internal" }
      statistic   = "Maximum"
      threshold   = 0
      period      = 300
      evaluations = 1
      missing     = "notBreaching"
    }
    rail-mismatch = {
      description = "The rail reported something that cannot be true for a payout; nothing was applied."
      metric      = "ferry_rail_mismatch_total"
      dimensions  = { service = "worker" }
      statistic   = "Sum"
      threshold   = 0
      period      = 300
      evaluations = 1
      missing     = "notBreaching"
    }
    credited-deposit-reorged-evm = {
      description = "A credited EVM deposit's block was reorged away; post a correction."
      metric      = "ferry_watcher_credited_reorged_total"
      dimensions  = { service = "watcher-evm" }
      statistic   = "Sum"
      threshold   = 0
      period      = 300
      evaluations = 1
      missing     = "notBreaching"
    }
    credited-deposit-reorged-solana = {
      description = "A credited Solana deposit's slot was rolled back; post a correction."
      metric      = "ferry_watcher_credited_reorged_total"
      dimensions  = { service = "watcher-solana" }
      statistic   = "Sum"
      threshold   = 0
      period      = 300
      evaluations = 1
      missing     = "notBreaching"
    }
    deep-reorg-evm = {
      description = "A reorg deeper than the stored history: the EVM watcher stopped."
      metric      = "ferry_watcher_deep_reorg_total"
      dimensions  = { service = "watcher-evm" }
      statistic   = "Sum"
      threshold   = 0
      period      = 300
      evaluations = 1
      missing     = "notBreaching"
    }
    outbox-lag = {
      description = "Events are not leaving the outbox (relay down or queue unreachable), or the API stopped reporting metrics."
      metric      = "ferry_outbox_lag_seconds"
      dimensions  = { service = "api" }
      statistic   = "Maximum"
      threshold   = 60
      period      = 60
      evaluations = 5
      missing     = "breaching"
    }
    recon-not-running = {
      description = "Reconciliation has not finished for over 26 hours."
      metric      = "ferry_recon_last_run_age_seconds"
      dimensions  = { service = "api" }
      statistic   = "Maximum"
      threshold   = 26 * 3600
      period      = 300
      evaluations = 1
      missing     = "notBreaching"
    }
    payouts-need-resolution = {
      description = "Payouts with an unknown submit outcome are waiting for the resolver."
      metric      = "ferry_payouts_needs_resolution"
      dimensions  = { service = "api" }
      statistic   = "Maximum"
      threshold   = 0
      period      = 300
      evaluations = 3
      missing     = "notBreaching"
    }
  }
}

resource "aws_cloudwatch_metric_alarm" "ferry" {
  for_each            = local.ferry
  alarm_name          = "${var.name}-${each.key}"
  alarm_description   = each.value.description
  namespace           = var.metrics_namespace
  metric_name         = each.value.metric
  dimensions          = each.value.dimensions
  statistic           = each.value.statistic
  comparison_operator = "GreaterThanThreshold"
  threshold           = each.value.threshold
  period              = each.value.period
  evaluation_periods  = each.value.evaluations
  treat_missing_data  = each.value.missing
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
}

resource "aws_cloudwatch_metric_alarm" "dlq" {
  alarm_name          = "${var.name}-dlq-not-empty"
  alarm_description   = "Payout events failed 5 deliveries and sit in the dead-letter queue."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = var.dlq_name }
  statistic           = "Maximum"
  comparison_operator = "GreaterThanThreshold"
  threshold           = 0
  period              = 300
  evaluation_periods  = 1
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
}

resource "aws_cloudwatch_metric_alarm" "api_5xx" {
  alarm_name          = "${var.name}-api-5xx"
  alarm_description   = "The API answered with more than 10 server errors in 5 minutes."
  namespace           = "AWS/ApplicationELB"
  metric_name         = "HTTPCode_Target_5XX_Count"
  dimensions          = { LoadBalancer = var.alb_arn_suffix, TargetGroup = var.target_group_arn_suffix }
  statistic           = "Sum"
  comparison_operator = "GreaterThanThreshold"
  threshold           = 10
  period              = 300
  evaluation_periods  = 1
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
}

resource "aws_cloudwatch_metric_alarm" "api_unhealthy" {
  alarm_name          = "${var.name}-api-unhealthy"
  alarm_description   = "No healthy API task behind the load balancer."
  namespace           = "AWS/ApplicationELB"
  metric_name         = "HealthyHostCount"
  dimensions          = { LoadBalancer = var.alb_arn_suffix, TargetGroup = var.target_group_arn_suffix }
  statistic           = "Minimum"
  comparison_operator = "LessThanThreshold"
  threshold           = 1
  period              = 60
  evaluation_periods  = 3
  treat_missing_data  = "breaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
}

resource "aws_budgets_budget" "monthly" {
  name         = "${var.name}-monthly"
  budget_type  = "COST"
  limit_amount = tostring(var.budget_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 80
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_email_addresses = [var.email]
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = [var.email]
  }
}
