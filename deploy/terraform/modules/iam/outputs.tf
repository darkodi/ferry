output "task_role_arns" {
  description = "Task role ARN by task name."
  value       = { for k, r in aws_iam_role.task : k => r.arn }
}

output "task_role_names" {
  description = "Task role name by task name."
  value       = { for k, r in aws_iam_role.task : k => r.name }
}

output "execution_role_arns" {
  description = "Execution role ARN by task name."
  value       = { for k, r in aws_iam_role.execution : k => r.arn }
}

output "execution_role_names" {
  description = "Execution role name by task name."
  value       = { for k, r in aws_iam_role.execution : k => r.name }
}
