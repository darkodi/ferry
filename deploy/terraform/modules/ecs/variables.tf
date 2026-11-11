variable "name" {
  description = "Name prefix, e.g. ferry-dev."
  type        = string
}

variable "region" {
  description = "AWS region."
  type        = string
}

variable "account_id" {
  description = "AWS account id."
  type        = string
}

variable "image_digest" {
  description = "Digest (sha256:...) of the verified Ferry image in this module's ECR repository."
  type        = string
  validation {
    condition     = can(regex("^sha256:[0-9a-f]{64}$", var.image_digest))
    error_message = "image_digest must be sha256:<64 hex>; deploys are pinned by digest, never by tag."
  }
}

variable "adot_image" {
  description = "ADOT collector image, pinned by digest."
  type        = string
}

variable "tasks" {
  description = "Ferry tasks by name. kind: service, scheduled (needs schedule) or oneoff. secrets maps env name to a Secrets Manager valueFrom. permissions are the task role's statements beyond tracing and metrics."
  type = map(object({
    kind          = string
    entrypoint    = string
    command       = optional(list(string), [])
    desired_count = optional(number, 1)
    singleton     = optional(bool, false)
    load_balanced = optional(bool, false)
    schedule      = optional(string)
    metrics_port  = optional(number)
    adot          = optional(bool, true)
    cpu           = optional(number, 256)
    memory        = optional(number, 512)
    environment   = optional(map(string), {})
    secrets       = optional(map(string), {})
    permissions = optional(list(object({
      actions   = list(string)
      resources = list(string)
    })), [])
  }))
  validation {
    condition     = alltrue([for t in values(var.tasks) : contains(["service", "scheduled", "oneoff"], t.kind)])
    error_message = "kind must be service, scheduled or oneoff."
  }
  validation {
    condition     = length([for t in values(var.tasks) : t if t.load_balanced]) <= 1
    error_message = "Only one task can sit behind the load balancer."
  }
}

variable "running" {
  description = "Run the services and the schedule. False keeps everything created but scaled to zero (first apply, before the image is pushed and migrated)."
  type        = bool
}

variable "task_role_arns" {
  description = "Task role ARN by task name (from the iam module)."
  type        = map(string)
}

variable "task_role_names" {
  description = "Task role name by task name."
  type        = map(string)
}

variable "execution_role_arns" {
  description = "Execution role ARN by task name."
  type        = map(string)
}

variable "execution_role_names" {
  description = "Execution role name by task name."
  type        = map(string)
}

variable "subnet_ids" {
  description = "Private subnets for the tasks."
  type        = list(string)
}

variable "api_security_group_id" {
  description = "Security group of the load-balanced task."
  type        = string
}

variable "tasks_security_group_id" {
  description = "Security group of every other task."
  type        = string
}

variable "target_group_arn" {
  description = "Load balancer target group of the API."
  type        = string
}

variable "kms_key_arn" {
  description = "Infra KMS key: logs, images, secrets."
  type        = string
}

variable "log_retention_days" {
  description = "Retention of task logs and metrics."
  type        = number
}

variable "deletion_protection" {
  description = "Keep the image repository on destroy."
  type        = bool
}
