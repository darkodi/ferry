output "queue_url" {
  description = "Payout queue URL."
  value       = aws_sqs_queue.payouts.url
}

output "queue_arn" {
  description = "Payout queue ARN."
  value       = aws_sqs_queue.payouts.arn
}

output "dlq_url" {
  description = "Dead-letter queue URL."
  value       = aws_sqs_queue.dlq.url
}

output "dlq_arn" {
  description = "Dead-letter queue ARN."
  value       = aws_sqs_queue.dlq.arn
}

output "dlq_name" {
  description = "Dead-letter queue name (for alarms)."
  value       = aws_sqs_queue.dlq.name
}
