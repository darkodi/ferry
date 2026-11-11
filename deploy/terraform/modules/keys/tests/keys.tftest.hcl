# Key policies: the account administers the application keys but cannot
# use them, and only the named roles can. Run: make tf-test
mock_provider "aws" {}

variables {
  name                = "ferry-test"
  account_id          = "123456789012"
  region              = "eu-west-1"
  noah_signers        = ["arn:aws:iam::123456789012:role/api", "arn:aws:iam::123456789012:role/worker"]
  solana_signers      = ["arn:aws:iam::123456789012:role/watcher-solana"]
  solana_readers      = ["arn:aws:iam::123456789012:role/api"]
  data_key_decrypters = ["arn:aws:iam::123456789012:role/api"]
}

run "policies" {
  command = apply

  assert {
    condition = alltrue([for p in [aws_kms_key.noah_signing.policy, aws_kms_key.solana_authority.policy, aws_kms_key.data.policy] :
      length(setintersection(
        toset(flatten([for st in jsondecode(p).Statement : st.Action if st.Principal.AWS == "arn:aws:iam::123456789012:root"])),
        toset(["kms:*", "kms:Sign", "kms:Decrypt", "kms:CreateGrant", "kms:Create*", "kms:Encrypt", "kms:GenerateDataKey"]),
    )) == 0])
    error_message = "The account root must not be able to sign, decrypt or grant with an application key."
  }
  assert {
    condition     = toset(flatten([for st in jsondecode(aws_kms_key.noah_signing.policy).Statement : st.Principal.AWS if contains(st.Action, "kms:Sign")])) == toset(var.noah_signers)
    error_message = "Only the Noah signers may sign with the Noah key."
  }
  assert {
    condition     = toset(flatten([for st in jsondecode(aws_kms_key.solana_authority.policy).Statement : st.Principal.AWS if contains(st.Action, "kms:Sign")])) == toset(var.solana_signers)
    error_message = "Only the Solana signers may sign with the authority key."
  }
  assert {
    condition     = toset(flatten([for st in jsondecode(aws_kms_key.data.policy).Statement : st.Principal.AWS if contains(st.Action, "kms:Decrypt")])) == toset(var.data_key_decrypters)
    error_message = "Only the named roles may unwrap the data key."
  }
  assert {
    condition     = aws_kms_key.noah_signing.customer_master_key_spec == "ECC_NIST_P384" && aws_kms_key.solana_authority.customer_master_key_spec == "ECC_NIST_EDWARDS25519"
    error_message = "Key specs must match what the signers check (P-384, Ed25519)."
  }
}
