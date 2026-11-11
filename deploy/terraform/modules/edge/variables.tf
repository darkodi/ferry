variable "name" {
  description = "Name prefix, e.g. ferry-dev."
  type        = string
}

variable "vpc_id" {
  description = "VPC id."
  type        = string
}

variable "public_subnet_ids" {
  description = "Public subnets for the load balancer."
  type        = list(string)
}

variable "security_group_id" {
  description = "Security group of the load balancer."
  type        = string
}

variable "certificate_arn" {
  description = "ACM certificate for the API's domain (in this region)."
  type        = string
}

variable "kms_key_arn" {
  description = "Infra KMS key for the WAF log group."
  type        = string
}

variable "log_retention_days" {
  description = "Retention of access and WAF logs."
  type        = number
}

variable "rate_limit_per_5min" {
  description = "Requests one IP may make in 5 minutes before WAF blocks it."
  type        = number
  default     = 1000
}

variable "deletion_protection" {
  description = "Protect the load balancer and keep its log bucket on destroy."
  type        = bool
}
