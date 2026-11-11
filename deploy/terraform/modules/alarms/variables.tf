variable "name" {
  description = "Name prefix, e.g. ferry-dev."
  type        = string
}

variable "email" {
  description = "Address that receives alarms and budget alerts (confirm the SNS subscription email)."
  type        = string
}

variable "kms_key_arn" {
  description = "Infra KMS key that encrypts the SNS topic."
  type        = string
}

variable "metrics_namespace" {
  description = "CloudWatch namespace of Ferry's metrics."
  type        = string
}

variable "dlq_name" {
  description = "Dead-letter queue name."
  type        = string
}

variable "alb_arn_suffix" {
  description = "Load balancer ARN suffix."
  type        = string
}

variable "target_group_arn_suffix" {
  description = "API target group ARN suffix."
  type        = string
}

variable "budget_usd" {
  description = "Monthly cost budget in USD."
  type        = number
}
