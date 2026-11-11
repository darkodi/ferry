variable "region" {
  description = "AWS region."
  type        = string
  default     = "eu-west-1"
}

variable "environment" {
  description = "Environment name; resources are named ferry-<environment>."
  type        = string
  default     = "dev"
}

variable "image_digest" {
  description = "Digest of the ghcr.io/darkodi/ferry image to deploy (verified with cosign and copied to ECR by make aws-up)."
  type        = string
}

variable "running" {
  description = "Run the services. make aws-up applies with false first, pushes the image and migrates, then applies with true."
  type        = bool
  default     = false
}

variable "certificate_arn" {
  description = "ACM certificate for the API's domain, in var.region."
  type        = string
}

variable "alarm_email" {
  description = "Email for alarms and budget alerts."
  type        = string
}

variable "budget_usd" {
  description = "Monthly budget in USD."
  type        = number
  default     = 150
}

variable "deletion_protection" {
  description = "Protect the database and load balancer. Off for a demo environment that make aws-down removes."
  type        = bool
  default     = false
}

variable "interface_endpoints" {
  description = "VPC interface endpoints for AWS APIs (keeps that traffic off the NAT; costs extra)."
  type        = bool
  default     = true
}

variable "noah_base_url" {
  description = "Noah API base URL."
  type        = string
  default     = "https://api.sandbox.noah.com/v1"
}

variable "noah_crypto_currency" {
  description = "Crypto currency code Noah sells (USDC_TEST in the sandbox)."
  type        = string
  default     = "USDC_TEST"
}

variable "evm" {
  description = "EVM deposits: null disables the EVM watcher and addresses. The RPC URL goes in the evm-rpc-url secret."
  type = object({
    chain         = string
    usdc_address  = string
    xpub          = string
    confirmations = optional(number, 12)
    start_block   = optional(number, 0)
  })
  default = null
}

variable "solana" {
  description = "Solana deposits: null disables the Solana watcher and addresses. The RPC URL goes in the solana-rpc-url secret; the authority is the KMS key this stack creates."
  type = object({
    chain         = string
    usdc_mint     = string
    deposit_owner = string
    start_slot    = optional(number, 0)
  })
  default = null
}

variable "log_retention_days" {
  description = "Retention of logs (task, flow, WAF, access) and metrics."
  type        = number
  default     = 365
}
