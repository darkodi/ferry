variable "name" {
  description = "Name prefix, e.g. ferry-dev."
  type        = string
}

variable "tasks" {
  description = "Task names; each gets a task role and an execution role."
  type        = list(string)
}

variable "account_id" {
  description = "AWS account id."
  type        = string
}

variable "region" {
  description = "AWS region."
  type        = string
}
