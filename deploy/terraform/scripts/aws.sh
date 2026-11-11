#!/usr/bin/env bash
# Deploys Ferry's dev environment to AWS: make aws-plan | aws-up | aws-down.
#
#   plan  verify the image and show what would change
#   up    verify the image (cosign), apply the stack scaled to zero, copy
#         the verified image to ECR by digest, fill empty secrets, run the
#         migrations, then start the services and the recon schedule
#   down  destroy the stack (asks for confirmation)
#
# up stops the services while it migrates, so a re-deploy has a short
# outage: acceptable for a demo environment, not for production (which
# would run expand-only migrations ahead of a rolling deploy).
#
# Needs: docker with buildx (Terraform runs in the pinned image), aws CLI v2, cosign,
# jq, AWS credentials for the target account in the environment or
# ~/.aws, and envs/dev/terraform.tfvars (see terraform.tfvars.example).
# Secrets it reads from the environment (make loads .env):
#   NOAH_API_KEY                         Noah API key for the environment
#   AWS_EVM_RPC_URL, AWS_SOLANA_RPC_URL  RPC endpoints, if those chains are on
# Nothing secret is printed, passed on a command line, or written to disk.
set -euo pipefail

cmd=${1:?usage: aws.sh plan|up|down}
here=$(cd "$(dirname "$0")/.." && pwd)
env_dir="$here/envs/dev"
: "${TF_IMAGE:?set by make}"
IMAGE=${FERRY_IMAGE:-ghcr.io/darkodi/ferry}
REF=${FERRY_IMAGE_REF:-main}
IDENTITY='^https://github.com/darkodi/ferry/\.github/workflows/ci\.yml@refs/heads/main$'
ISSUER=https://token.actions.githubusercontent.com

for tool in docker aws cosign jq; do
  command -v "$tool" >/dev/null || { echo "aws.sh: $tool is required" >&2; exit 1; }
done
[[ -f "$env_dir/terraform.tfvars" ]] || { echo "aws.sh: create $env_dir/terraform.tfvars from terraform.tfvars.example" >&2; exit 1; }

# Terraform in the pinned image, with the caller's AWS credentials.
tf() {
  docker run --rm -i -u "$(id -u):$(id -g)" -e HOME=/tmp \
    -e AWS_PROFILE -e AWS_REGION -e AWS_DEFAULT_REGION \
    -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN \
    -v "${HOME}/.aws:/tmp/.aws:ro" -v "$here:/w" -w /w/envs/dev "$TF_IMAGE" "$@"
}

# The digest of the image CI signed on main. cosign checks the signature
# was made by this repository's CI workflow on main (keyless, GitHub OIDC,
# recorded in the Rekor transparency log) before anything is deployed.
verified_digest() {
  cosign verify "$IMAGE:$REF" \
    --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER" \
    --output json 2>/dev/null | jq -er '.[0].critical.image."docker-manifest-digest"'
}

# put_if_empty <secret arn> <name>: reads the value from stdin and stores it
# only when the secret has no value yet, so re-running never rotates.
has_value() {
  [[ "$(aws secretsmanager describe-secret --secret-id "$1" --query 'VersionIdsToStages' --output text)" != "None" ]]
}
put_from_stdin() {
  aws secretsmanager put-secret-value --secret-id "$1" --secret-string file:///dev/stdin >/dev/null
  echo "  $2: set"
}

tf init -input=false -lockfile=readonly >/dev/null

case "$cmd" in
plan)
  digest=$(verified_digest)
  echo "image $IMAGE@$digest (signature verified)"
  tf plan -input=false -var "image_digest=$digest" -var running=true
  ;;

up)
  digest=$(verified_digest)
  echo "== image $IMAGE@$digest (signature verified)"

  echo "== stack (services stopped)"
  tf apply -input=false -auto-approve -var "image_digest=$digest" -var running=false
  out=$(tf output -json)
  o() { jq -er "$1" <<<"$out"; }
  ob() { jq -r "$1" <<<"$out"; } # booleans: jq -e would fail on false
  repo=$(o .repository_url.value)
  region=$(o .region.value)
  export AWS_REGION=$region

  echo "== copy image to ECR by digest"
  if aws ecr describe-images --repository-name "${repo#*/}" --image-ids "imageDigest=$digest" >/dev/null 2>&1; then
    echo "  already present"
  else
    aws ecr get-login-password | docker login --username AWS --password-stdin "${repo%%/*}" >/dev/null
    # Copies the image index as-is, so the digest (what CI signed) is kept.
    docker buildx imagetools create --tag "$repo:${digest#sha256:}" "$IMAGE@$digest" >/dev/null
  fi
  # The tasks run "$repo@$digest": the exact bytes whose signature was
  # verified above. Refuse to go on if ECR holds anything else.
  aws ecr describe-images --repository-name "${repo#*/}" --image-ids "imageDigest=$digest" >/dev/null \
    || { echo "aws.sh: $digest is not in ECR after the copy" >&2; exit 1; }

  echo "== secrets"
  sec() { o ".secret_arns.value[\"$1\"]"; }
  for name in admin-key app-db-password; do
    arn=$(sec "$name")
    has_value "$arn" && { echo "  $name: kept"; continue; }
    aws secretsmanager get-random-password --password-length 48 --exclude-punctuation \
      --require-each-included-type --query RandomPassword --output text | put_from_stdin "$arn" "$name"
  done
  arn=$(sec data-key-encrypted)
  if has_value "$arn"; then echo "  data-key-encrypted: kept"; else
    # A new 256-bit key, returned only wrapped: nobody sees the plaintext.
    aws kms generate-data-key-without-plaintext --key-id "$(o .data_key_arn.value)" --key-spec AES_256 \
      --query CiphertextBlob --output text | put_from_stdin "$arn" data-key-encrypted
  fi
  operator_secret() { # <secret name> <env var> <required>
    local arn; arn=$(sec "$1")
    if has_value "$arn"; then echo "  $1: kept"; return; fi
    [[ "$3" == true ]] || { echo "  $1: not used"; return; }
    [[ -n "${!2:-}" ]] || { echo "aws.sh: $2 is empty; set it in .env" >&2; exit 1; }
    printf %s "${!2}" | put_from_stdin "$arn" "$1"
  }
  operator_secret noah-api-key NOAH_API_KEY true
  operator_secret evm-rpc-url AWS_EVM_RPC_URL "$(ob .chains.value.evm)"
  operator_secret solana-rpc-url AWS_SOLANA_RPC_URL "$(ob .chains.value.solana)"

  echo "== migrate"
  subnets=$(o '.private_subnet_ids.value | join(",")')
  sg=$(o .tasks_security_group_id.value)
  cluster=$(o .cluster.value)
  task=$(aws ecs run-task --cluster "$cluster" --task-definition "$(o .migrate_task_definition.value)" \
    --launch-type FARGATE --count 1 \
    --network-configuration "awsvpcConfiguration={subnets=[$subnets],securityGroups=[$sg],assignPublicIp=DISABLED}" \
    --query 'tasks[0].taskArn' --output text)
  aws ecs wait tasks-stopped --cluster "$cluster" --tasks "$task"
  # shellcheck disable=SC2016 # backticks are a JMESPath literal, not a shell expansion
  code=$(aws ecs describe-tasks --cluster "$cluster" --tasks "$task" \
    --query 'tasks[0].containers[?name==`migrate`].exitCode | [0]' --output text)
  [[ "$code" == 0 ]] || { echo "aws.sh: migrate exited with $code; see log group /$(o .name.value)/migrate" >&2; exit 1; }
  echo "  migrations applied, app login ready"

  echo "== start services"
  tf apply -input=false -auto-approve -var "image_digest=$digest" -var running=true
  echo
  echo "Ferry is starting. Point your API domain at $(tf output -raw api_dns_name),"
  echo "and confirm the SNS subscription email to receive alarms."
  ;;

down)
  # The digest does not matter when destroying.
  tf destroy -input=false -var "image_digest=sha256:$(printf '0%.0s' {1..64})"
  ;;

*)
  echo "usage: aws.sh plan|up|down" >&2
  exit 2
  ;;
esac
