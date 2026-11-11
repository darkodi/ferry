variable "name" {
  description = "Name prefix, e.g. ferry-dev."
  type        = string
}

variable "cidr" {
  description = "VPC CIDR block (a /16; subnets are /24s)."
  type        = string
}

variable "region" {
  description = "AWS region."
  type        = string
}

variable "account_id" {
  description = "AWS account id."
  type        = string
}

variable "kms_key_arn" {
  description = "KMS key that encrypts the flow log group."
  type        = string
}

variable "log_retention_days" {
  description = "Retention of the VPC flow logs."
  type        = number
}

variable "interface_endpoints" {
  description = "Create interface endpoints for AWS APIs (about 7 USD a month each per AZ). Off routes that traffic through the NAT."
  type        = bool
  default     = true
}

variable "availability_zones" {
  description = "The two availability zones to use; null picks <region>a and <region>b."
  type        = list(string)
  default     = null
  validation {
    condition     = var.availability_zones == null || length(coalesce(var.availability_zones, [])) == 2
    error_message = "Give exactly two availability zones."
  }
}
