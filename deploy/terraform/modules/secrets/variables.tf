variable "name" {
  description = "Name prefix, e.g. ferry-dev."
  type        = string
}

variable "kms_key_arn" {
  description = "Infra KMS key that encrypts the secrets."
  type        = string
}

variable "recovery_window_days" {
  description = "Days a deleted secret can be restored (0 deletes at once)."
  type        = number
  default     = 7
}
