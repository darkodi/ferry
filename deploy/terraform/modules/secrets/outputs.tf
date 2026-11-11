output "arns" {
  description = "Secret ARN by name: admin-key, app-db-password, noah-api-key, data-key-encrypted, evm-rpc-url, solana-rpc-url."
  value       = { for k, s in aws_secretsmanager_secret.this : k => s.arn }
}
