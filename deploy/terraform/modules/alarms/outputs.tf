output "topic_arn" {
  description = "SNS topic the alarms notify."
  value       = aws_sns_topic.alarms.arn
}
