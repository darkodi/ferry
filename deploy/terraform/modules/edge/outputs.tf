output "target_group_arn" {
  description = "Target group the API service registers with."
  value       = aws_lb_target_group.api.arn
}

output "alb_dns_name" {
  description = "Load balancer DNS name (point the API's domain here)."
  value       = aws_lb.this.dns_name
}

output "alb_arn_suffix" {
  description = "Load balancer ARN suffix (for alarms)."
  value       = aws_lb.this.arn_suffix
}

output "target_group_arn_suffix" {
  description = "Target group ARN suffix (for alarms)."
  value       = aws_lb_target_group.api.arn_suffix
}
