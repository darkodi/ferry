output "vpc_id" {
  description = "VPC id."
  value       = aws_vpc.this.id
}

output "public_subnet_ids" {
  description = "Public subnets (load balancer, NAT)."
  value       = aws_subnet.public[*].id
}

output "private_subnet_ids" {
  description = "Private subnets (tasks, database, endpoints)."
  value       = aws_subnet.private[*].id
}

output "security_group_ids" {
  description = "Security group id by role: alb, api, tasks, db."
  value = {
    alb   = aws_security_group.alb.id
    api   = aws_security_group.api.id
    tasks = aws_security_group.tasks.id
    db    = aws_security_group.db.id
  }
}
