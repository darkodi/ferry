# Ferry on AWS

Terraform for Ferry's `dev` environment. The design and the reasoning behind it are in [`docs/design.md`](../../docs/design.md) (Infrastructure, "As built (M7)"), and the threats are in [`docs/threat-model.md`](../../docs/threat-model.md) ("AWS deployment").

```text
envs/dev/     the environment: which tasks run, with which environment, secrets and permissions
modules/
  iam/        a task role and an execution role per task
  keys/       KMS: Noah signing (P-384), Solana authority (Ed25519), data key, infra key
  network/    VPC, subnets, NAT, flow logs, security groups, VPC endpoints
  data/       RDS Postgres 16
  queue/      SQS payouts + DLQ
  secrets/    Secrets Manager entries (values are filled by make aws-up, never by Terraform)
  ecs/        ECR, cluster, task definitions with ADOT sidecars, services, recon schedule
  edge/       ALB, WAF, access logs
  alarms/     SNS, CloudWatch alarms, budget
scripts/aws.sh  make aws-plan | aws-up | aws-down
```

## Check (no AWS account needed)

```bash
make tf-check   # fmt, validate, terraform test (mocked AWS), tflint, checkov
```

The tools run in containers pinned by digest, so only Docker is needed. CI runs the same target.

## Deploy

You need:

- Docker with buildx, the AWS CLI v2, cosign and jq;
- credentials for the target account (`AWS_PROFILE` or the usual variables);
- an ACM certificate for the API's domain, in the target region;
- `ghcr.io/darkodi/ferry` must be readable: make the GHCR package public, or `docker login ghcr.io` first.

1. Copy `envs/dev/terraform.tfvars.example` to `envs/dev/terraform.tfvars` (git-ignored) and fill it in.
2. In `.env`, set `NOAH_API_KEY`, plus `AWS_EVM_RPC_URL` and `AWS_SOLANA_RPC_URL` for the chains you enabled.
3. Run `make aws-plan` to review, then `make aws-up`. The script:
   1. verifies the `main` image's cosign signature (made by this repository's CI) and takes its digest;
   2. applies the stack with the services stopped;
   3. copies the image to ECR by digest;
   4. fills the empty secrets (generating the admin key, the database password and a KMS-wrapped data key);
   5. runs migrations as the database owner;
   6. starts the services and the daily recon schedule.
4. Point the API's domain at the `api_dns_name` output, and confirm the SNS subscription email.
5. Read the admin key with `aws secretsmanager get-secret-value --secret-id ferry-dev/admin-key`.

`make aws-down` destroys everything. KMS keys enter their deletion window rather than disappearing at once.

Running, the stack costs roughly 100 to 150 USD a month, mostly the NAT gateway, interface endpoints, RDS and the ALB. The budget alert defaults to 150 USD. Tear it down when the demo is over.
