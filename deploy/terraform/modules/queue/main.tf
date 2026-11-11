# The payout queue and its dead-letter queue, matching deploy/elasticmq.conf:
# after 5 failed deliveries (queue.MaxReceives) a message moves to the DLQ,
# where an alarm fires. Encrypted with SQS-managed keys, and the queue
# policies refuse any request not made over TLS.

resource "aws_sqs_queue" "dlq" {
  name                      = "${var.name}-payouts-dlq"
  message_retention_seconds = 1209600 # 14 days, the maximum: time to investigate
  sqs_managed_sse_enabled   = true
}

resource "aws_sqs_queue" "payouts" {
  name                       = "${var.name}-payouts"
  visibility_timeout_seconds = 30
  receive_wait_time_seconds  = 20
  message_retention_seconds  = 345600 # 4 days
  sqs_managed_sse_enabled    = true
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dlq.arn
    maxReceiveCount     = 5
  })
}

resource "aws_sqs_queue_redrive_allow_policy" "dlq" {
  queue_url = aws_sqs_queue.dlq.id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.payouts.arn]
  })
}

resource "aws_sqs_queue_policy" "tls_only" {
  for_each  = { payouts = aws_sqs_queue.payouts, dlq = aws_sqs_queue.dlq }
  queue_url = each.value.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "DenyInsecureTransport"
      Effect    = "Deny"
      Principal = "*"
      Action    = "sqs:*"
      Resource  = each.value.arn
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })
}
