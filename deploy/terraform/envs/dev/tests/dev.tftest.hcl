# Applies the whole dev environment against a mocked AWS provider (no
# credentials, nothing created) and checks the least-privilege table in
# main.tf. Run: make tf-test
mock_provider "aws" {
  source = "./tests"
}

# Distinct ARNs per key, so a mix-up between keys fails the checks.
override_resource {
  target = module.keys.aws_kms_key.noah_signing
  values = { arn = "arn:aws:kms:eu-west-1:123456789012:key/noah" }
}
override_resource {
  target = module.keys.aws_kms_key.solana_authority
  values = { arn = "arn:aws:kms:eu-west-1:123456789012:key/solana" }
}
override_resource {
  target = module.keys.aws_kms_key.data
  values = { arn = "arn:aws:kms:eu-west-1:123456789012:key/data" }
}
override_resource {
  target = module.keys.aws_kms_key.infra
  values = { arn = "arn:aws:kms:eu-west-1:123456789012:key/infra" }
}

variables {
  image_digest    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
  certificate_arn = "arn:aws:acm:eu-west-1:123456789012:certificate/mock"
  alarm_email     = "alerts@example.com"
  running         = true
  evm = {
    chain        = "base-sepolia"
    usdc_address = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
    xpub         = "xpub-mock"
  }
  solana = {
    chain         = "solana-devnet"
    usdc_mint     = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
    deposit_owner = "Owner1111111111111111111111111111111111111"
  }
}

run "least_privilege" {
  command = apply

  assert {
    condition = toset([for k, t in local.tasks : k if anytrue([for p in lookup(t, "permissions", []) :
    contains(p.actions, "kms:Decrypt") && contains(p.resources, module.keys.data_key_arn)])]) == toset(["api"])
    error_message = "Only the API may unwrap the data key."
  }
  assert {
    condition = toset([for k, t in local.tasks : k if anytrue([for p in lookup(t, "permissions", []) :
    contains(p.actions, "kms:Sign") && contains(p.resources, module.keys.noah_signing_key_arn)])]) == toset(["api", "recon", "worker"])
    error_message = "Only the API, worker and recon may sign Noah requests."
  }
  assert {
    condition = toset([for k, t in local.tasks : k if anytrue([for p in lookup(t, "permissions", []) :
    contains(p.actions, "kms:Sign") && contains(p.resources, module.keys.solana_authority_key_arn)])]) == toset(["watcher-solana"])
    error_message = "Only the Solana watcher may sign with the authority key."
  }
  assert {
    condition     = toset([for k, t in local.tasks : k if anytrue([for v in values(t.secrets) : strcontains(v, "rds!")])]) == toset(["migrate"])
    error_message = "Only migrate may read the database owner's credentials."
  }
  assert {
    condition     = toset([for k, t in local.tasks : k if contains(keys(t.secrets), "FERRY_ADMIN_KEY") || contains(keys(t.secrets), "FERRY_DATA_KEY_ENCRYPTED")]) == toset(["api"])
    error_message = "The admin key and the wrapped data key go to the API only."
  }
  assert {
    condition = alltrue([for t in values(local.tasks) : alltrue([for k in keys(t.environment) :
    !contains(["PGPASSWORD", "NOAH_API_KEY", "FERRY_ADMIN_KEY", "FERRY_DATA_KEY", "FERRY_APP_DB_PASSWORD"], k)])])
    error_message = "A secret is set as a plain environment variable."
  }
  assert {
    condition     = alltrue([for t in values(local.tasks) : strcontains(lookup(t.environment, "DATABASE_URL", "sslmode=verify-full"), "sslmode=verify-full")])
    error_message = "Every task must verify the database certificate."
  }
  assert {
    condition     = toset([for k, t in local.tasks : k if t.kind == "service" && lookup(t, "load_balanced", false)]) == toset(["api"])
    error_message = "Only the API sits behind the load balancer."
  }
}

run "no_chains" {
  command = apply
  variables {
    evm    = null
    solana = null
  }
  assert {
    condition     = toset(keys(local.tasks)) == toset(["api", "migrate", "recon", "relay", "worker"])
    error_message = "Unconfigured chains must not get a watcher."
  }
}
