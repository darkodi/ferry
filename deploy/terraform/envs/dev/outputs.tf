output "api_dns_name" {
  description = "Point the API's domain (the certificate's name) at this load balancer."
  value       = module.edge.alb_dns_name
}

output "repository_url" {
  description = "ECR repository make aws-up copies the verified image to."
  value       = module.ecs.repository_url
}

output "cluster" {
  description = "ECS cluster name."
  value       = module.ecs.cluster_name
}

output "migrate_task_definition" {
  description = "Task definition make aws-up runs before starting the services."
  value       = module.ecs.task_definition_arns["migrate"]
}

output "private_subnet_ids" {
  description = "Subnets for one-off tasks."
  value       = module.network.private_subnet_ids
}

output "tasks_security_group_id" {
  description = "Security group for one-off tasks."
  value       = module.network.security_group_ids.tasks
}

output "data_key_arn" {
  description = "KMS key make aws-up wraps a new data key with."
  value       = module.keys.data_key_arn
}

output "secret_arns" {
  description = "Secrets Manager secret ARN by name."
  value       = module.secrets.arns
}

output "region" {
  description = "AWS region of the stack."
  value       = var.region
}

output "name" {
  description = "Name prefix of every resource."
  value       = local.name
}

output "chains" {
  description = "Which deposit chains are configured."
  value       = { evm = var.evm != null, solana = var.solana != null }
}
