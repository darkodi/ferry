output "cluster_name" {
  description = "ECS cluster name."
  value       = aws_ecs_cluster.this.name
}

output "repository_url" {
  description = "ECR repository the image is copied to."
  value       = aws_ecr_repository.this.repository_url
}

output "task_definition_arns" {
  description = "Task definition ARN by task name."
  value       = { for k, t in aws_ecs_task_definition.this : k => t.arn }
}

output "metrics_namespace" {
  description = "CloudWatch namespace of Ferry's metrics."
  value       = local.metrics_ns
}
