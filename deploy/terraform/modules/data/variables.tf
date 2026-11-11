variable "name" {
  description = "Name prefix, e.g. ferry-dev."
  type        = string
}

variable "subnet_ids" {
  description = "Private subnets for the database."
  type        = list(string)
}

variable "security_group_id" {
  description = "Security group of the database."
  type        = string
}

variable "kms_key_arn" {
  description = "Infra KMS key: storage, Performance Insights and the master password secret."
  type        = string
}

variable "instance_class" {
  description = "RDS instance class."
  type        = string
  default     = "db.t4g.micro"
}

variable "multi_az" {
  description = "Standby in a second AZ (doubles the cost)."
  type        = bool
  default     = false
}

variable "deletion_protection" {
  description = "Protect the database from deletion and take a final snapshot. Off lets `make aws-down` remove a demo environment."
  type        = bool
}
