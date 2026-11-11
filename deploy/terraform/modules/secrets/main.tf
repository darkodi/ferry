# Secrets the tasks receive as environment variables, injected by ECS from
# Secrets Manager. Terraform creates the secrets but never their values, so
# no secret is in Terraform state or the task definitions. make aws-up
# (scripts/aws-up.sh) fills each one that is empty:
#
#   admin-key, app-db-password  random, from Secrets Manager's generator
#                               (letters and digits: migrate -app-login
#                               writes the password into SQL and accepts
#                               nothing else)
#   data-key-encrypted          a new data key wrapped by KMS
#                               (GenerateDataKeyWithoutPlaintext: nobody
#                               sees the plaintext)
#   noah-api-key, *-rpc-url     from the operator's .env
#
# The services do not start while one of them is empty.

resource "aws_secretsmanager_secret" "this" {
  #checkov:skip=CKV2_AWS_57:No rotation Lambda. Noah and RPC keys are issued by third parties; rotation is a new value put by the operator plus make aws-up (docs/threat-model.md, AWS deployment).
  for_each = toset([
    "admin-key", "app-db-password", "data-key-encrypted",
    "noah-api-key", "evm-rpc-url", "solana-rpc-url",
  ])
  name                    = "${var.name}/${each.key}"
  kms_key_id              = var.kms_key_arn
  recovery_window_in_days = var.recovery_window_days
}
