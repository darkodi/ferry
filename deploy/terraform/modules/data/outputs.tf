output "address" {
  description = "Database host name."
  value       = aws_db_instance.this.address
}

output "port" {
  description = "Database port."
  value       = aws_db_instance.this.port
}

output "master_secret_arn" {
  description = "Secrets Manager secret (JSON: username, password) of the master user, managed by RDS."
  value       = aws_db_instance.this.master_user_secret[0].secret_arn
}
