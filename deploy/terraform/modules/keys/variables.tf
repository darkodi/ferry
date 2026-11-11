variable "name" {
  description = "Name prefix, e.g. ferry-dev."
  type        = string
}

variable "account_id" {
  description = "AWS account id."
  type        = string
}

variable "region" {
  description = "AWS region."
  type        = string
}

variable "noah_signers" {
  description = "Role ARNs that may sign Noah requests with the P-384 key."
  type        = list(string)
}

variable "solana_signers" {
  description = "Role ARNs that may sign with the Solana authority key."
  type        = list(string)
}

variable "solana_readers" {
  description = "Role ARNs that may read the Solana authority's public key (to derive deposit addresses)."
  type        = list(string)
}

variable "data_key_decrypters" {
  description = "Role ARNs that may unwrap the bank-details data key."
  type        = list(string)
}

variable "deletion_window_days" {
  description = "Days a deleted key can still be recovered (7 to 30)."
  type        = number
  default     = 30
}
