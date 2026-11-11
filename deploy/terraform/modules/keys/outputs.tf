output "noah_signing_key_arn" {
  description = "ARN of the Noah request signing key."
  value       = aws_kms_key.noah_signing.arn
}

output "solana_authority_key_arn" {
  description = "ARN of the Solana authority key."
  value       = aws_kms_key.solana_authority.arn
}

output "data_key_arn" {
  description = "ARN of the key that wraps the bank-details data key."
  value       = aws_kms_key.data.arn
}

output "infra_key_arn" {
  description = "ARN of the infrastructure encryption key."
  value       = aws_kms_key.infra.arn
}
