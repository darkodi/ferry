# Ferry's dev environment on AWS. Each task's environment, secrets and
# permissions are listed in local.tasks below; that map is the whole
# least-privilege story in one place:
#
#   task            signs Noah  signs Solana  unwraps data key  queue
#   api             yes         public key    yes               -
#   relay           -           -             -                 send
#   worker          yes         -             -                 receive, delete
#   watcher-evm     -           -             -                 -
#   watcher-solana  -           yes           -                 -
#   recon           yes         -             -                 -
#   migrate         -           -             -                 -  (database owner)

data "aws_caller_identity" "current" {}

locals {
  name       = "ferry-${var.environment}"
  account_id = data.aws_caller_identity.current.account_id
  secret     = module.secrets.arns
  app_login  = "ferry_svc"

  # Services connect as the app login (member of ferry_app) and verify the
  # database certificate against the RDS CA bundle in the image.
  db_env = {
    DATABASE_URL = "postgres:///ferry?sslmode=verify-full&sslrootcert=/ferry/rds-ca.pem"
    PGHOST       = module.data.address
    PGPORT       = tostring(module.data.port)
    PGUSER       = local.app_login
  }
  obs_env = {
    AWS_REGION                  = var.region
    FERRY_METRICS_ADDR          = "127.0.0.1:9100"
    OTEL_EXPORTER_OTLP_ENDPOINT = "http://127.0.0.1:4318"
  }
  noah_env = {
    FERRY_RAIL              = "noah"
    NOAH_BASE_URL           = var.noah_base_url
    NOAH_CRYPTO_CURRENCY    = var.noah_crypto_currency
    NOAH_SIGNING_KMS_KEY_ID = module.keys.noah_signing_key_arn
  }
  evm_env = var.evm == null ? {} : {
    FERRY_EVM_CHAIN         = var.evm.chain
    FERRY_EVM_USDC_ADDRESS  = var.evm.usdc_address
    FERRY_EVM_CONFIRMATIONS = tostring(var.evm.confirmations)
  }
  solana_env = var.solana == null ? {} : {
    FERRY_SOLANA_CHAIN = var.solana.chain
  }

  sign_noah   = { actions = ["kms:Sign", "kms:GetPublicKey", "kms:DescribeKey"], resources = [module.keys.noah_signing_key_arn] }
  sign_solana = { actions = ["kms:Sign", "kms:GetPublicKey", "kms:DescribeKey"], resources = [module.keys.solana_authority_key_arn] }

  all_tasks = {
    api = {
      kind          = "service"
      entrypoint    = "/ferry/api"
      load_balanced = true
      metrics_port  = 9100
      environment = merge(local.db_env, local.obs_env, local.noah_env, local.evm_env, local.solana_env,
        { FERRY_ADDR = "0.0.0.0:8080" },
        var.evm == null ? {} : { FERRY_EVM_XPUB = var.evm.xpub },
        var.solana == null ? {} : { FERRY_SOLANA_AUTHORITY_KMS_KEY_ID = module.keys.solana_authority_key_arn },
      )
      secrets = {
        PGPASSWORD               = local.secret["app-db-password"]
        FERRY_ADMIN_KEY          = local.secret["admin-key"]
        NOAH_API_KEY             = local.secret["noah-api-key"]
        FERRY_DATA_KEY_ENCRYPTED = local.secret["data-key-encrypted"]
      }
      permissions = concat(
        [local.sign_noah, { actions = ["kms:Decrypt"], resources = [module.keys.data_key_arn] }],
        var.solana == null ? [] : [{ actions = ["kms:GetPublicKey", "kms:DescribeKey"], resources = [module.keys.solana_authority_key_arn] }],
      )
    }
    relay = {
      kind         = "service"
      entrypoint   = "/ferry/relay"
      metrics_port = 9100
      environment  = merge(local.db_env, local.obs_env, { FERRY_SQS_QUEUE_URL = module.queue.queue_url })
      secrets      = { PGPASSWORD = local.secret["app-db-password"] }
      permissions  = [{ actions = ["sqs:SendMessage"], resources = [module.queue.queue_arn] }]
    }
    worker = {
      kind         = "service"
      entrypoint   = "/ferry/worker"
      metrics_port = 9100
      environment = merge(local.db_env, local.obs_env, local.noah_env, {
        FERRY_SQS_QUEUE_URL = module.queue.queue_url
        FERRY_SQS_DLQ_URL   = module.queue.dlq_url
      })
      secrets = {
        PGPASSWORD   = local.secret["app-db-password"]
        NOAH_API_KEY = local.secret["noah-api-key"]
      }
      permissions = [
        local.sign_noah,
        { actions = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes"], resources = [module.queue.queue_arn] },
        { actions = ["sqs:GetQueueAttributes"], resources = [module.queue.dlq_arn] },
      ]
    }
    watcher-evm = {
      kind         = "service"
      entrypoint   = "/ferry/watcher"
      singleton    = true
      metrics_port = 9100
      environment = merge(local.db_env, local.obs_env, local.evm_env, {
        FERRY_CHAIN           = "evm"
        FERRY_EVM_START_BLOCK = var.evm == null ? "0" : tostring(var.evm.start_block)
      })
      secrets = {
        PGPASSWORD        = local.secret["app-db-password"]
        FERRY_EVM_RPC_URL = local.secret["evm-rpc-url"]
      }
    }
    watcher-solana = {
      kind         = "service"
      entrypoint   = "/ferry/watcher"
      singleton    = true
      metrics_port = 9100
      environment = merge(local.db_env, local.obs_env, local.solana_env, var.solana == null ? {} : {
        FERRY_CHAIN                       = "solana"
        FERRY_SOLANA_USDC_MINT            = var.solana.usdc_mint
        FERRY_SOLANA_DEPOSIT_OWNER        = var.solana.deposit_owner
        FERRY_SOLANA_START_SLOT           = tostring(var.solana.start_slot)
        FERRY_SOLANA_AUTHORITY_KMS_KEY_ID = module.keys.solana_authority_key_arn
      })
      secrets = {
        PGPASSWORD           = local.secret["app-db-password"]
        FERRY_SOLANA_RPC_URL = local.secret["solana-rpc-url"]
      }
      permissions = [local.sign_solana]
    }
    recon = {
      kind        = "scheduled"
      entrypoint  = "/ferry/recon"
      schedule    = "cron(15 3 * * ? *)" # daily, 03:15 UTC
      environment = merge(local.db_env, local.obs_env, local.noah_env, local.evm_env, local.solana_env)
      secrets = merge(
        { PGPASSWORD = local.secret["app-db-password"], NOAH_API_KEY = local.secret["noah-api-key"] },
        var.evm == null ? {} : { FERRY_EVM_RPC_URL = local.secret["evm-rpc-url"] },
        var.solana == null ? {} : { FERRY_SOLANA_RPC_URL = local.secret["solana-rpc-url"] },
      )
      permissions = [local.sign_noah]
    }
    # Runs as the database owner (RDS-managed master secret) and creates or
    # updates the app login. Started only by make aws-up.
    migrate = {
      kind       = "oneoff"
      entrypoint = "/ferry/migrate"
      command    = ["-app-login", local.app_login]
      adot       = false
      environment = {
        DATABASE_URL = local.db_env.DATABASE_URL
        PGHOST       = module.data.address
        PGPORT       = tostring(module.data.port)
      }
      secrets = {
        PGUSER                = "${module.data.master_secret_arn}:username::"
        PGPASSWORD            = "${module.data.master_secret_arn}:password::"
        FERRY_APP_DB_PASSWORD = local.secret["app-db-password"]
      }
    }
  }

  # Chains that are not configured get no watcher.
  tasks = { for k, t in local.all_tasks : k => t if !(k == "watcher-evm" && var.evm == null) && !(k == "watcher-solana" && var.solana == null) }
  task_names = [
    "api", "relay", "worker", "recon", "migrate",
    var.evm == null ? "" : "watcher-evm", var.solana == null ? "" : "watcher-solana",
  ]
}

module "iam" {
  source     = "../../modules/iam"
  name       = local.name
  tasks      = compact(local.task_names)
  account_id = local.account_id
  region     = var.region
}

module "keys" {
  source              = "../../modules/keys"
  name                = local.name
  account_id          = local.account_id
  region              = var.region
  noah_signers        = [for t in ["api", "worker", "recon"] : module.iam.task_role_arns[t]]
  solana_signers      = var.solana == null ? [] : [module.iam.task_role_arns["watcher-solana"]]
  solana_readers      = var.solana == null ? [] : [module.iam.task_role_arns["api"]]
  data_key_decrypters = [module.iam.task_role_arns["api"]]
}

module "network" {
  source              = "../../modules/network"
  name                = local.name
  cidr                = "10.40.0.0/16"
  region              = var.region
  account_id          = local.account_id
  kms_key_arn         = module.keys.infra_key_arn
  log_retention_days  = var.log_retention_days
  interface_endpoints = var.interface_endpoints
}

module "data" {
  source              = "../../modules/data"
  name                = local.name
  subnet_ids          = module.network.private_subnet_ids
  security_group_id   = module.network.security_group_ids.db
  kms_key_arn         = module.keys.infra_key_arn
  deletion_protection = var.deletion_protection
}

module "queue" {
  source = "../../modules/queue"
  name   = local.name
}

module "secrets" {
  source      = "../../modules/secrets"
  name        = local.name
  kms_key_arn = module.keys.infra_key_arn
}

module "edge" {
  source              = "../../modules/edge"
  name                = local.name
  vpc_id              = module.network.vpc_id
  public_subnet_ids   = module.network.public_subnet_ids
  security_group_id   = module.network.security_group_ids.alb
  certificate_arn     = var.certificate_arn
  kms_key_arn         = module.keys.infra_key_arn
  log_retention_days  = var.log_retention_days
  deletion_protection = var.deletion_protection
}

module "ecs" {
  source                  = "../../modules/ecs"
  name                    = local.name
  region                  = var.region
  account_id              = local.account_id
  image_digest            = var.image_digest
  adot_image              = "public.ecr.aws/aws-observability/aws-otel-collector:v0.50.0@sha256:7968fb60db6a2390a47ba6a2df029745638486e285c9b2487da1b722d0855a3e"
  tasks                   = local.tasks
  running                 = var.running
  task_role_arns          = module.iam.task_role_arns
  task_role_names         = module.iam.task_role_names
  execution_role_arns     = module.iam.execution_role_arns
  execution_role_names    = module.iam.execution_role_names
  subnet_ids              = module.network.private_subnet_ids
  api_security_group_id   = module.network.security_group_ids.api
  tasks_security_group_id = module.network.security_group_ids.tasks
  target_group_arn        = module.edge.target_group_arn
  kms_key_arn             = module.keys.infra_key_arn
  log_retention_days      = var.log_retention_days
  deletion_protection     = var.deletion_protection
}

module "alarms" {
  source                  = "../../modules/alarms"
  name                    = local.name
  email                   = var.alarm_email
  kms_key_arn             = module.keys.infra_key_arn
  metrics_namespace       = module.ecs.metrics_namespace
  dlq_name                = module.queue.dlq_name
  alb_arn_suffix          = module.edge.alb_arn_suffix
  target_group_arn_suffix = module.edge.target_group_arn_suffix
  budget_usd              = var.budget_usd
}
