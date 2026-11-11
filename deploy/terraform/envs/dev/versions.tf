terraform {
  required_version = ">= 1.11" # checked with 1.16 (make tf-check)

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "6.70.0"
    }
  }

  # State holds no secret values (Terraform creates secrets but not their
  # values, and RDS manages the master password), but it maps the account.
  # For a shared environment use a remote backend, e.g.:
  #
  #   backend "s3" {
  #     bucket       = "<state bucket>"
  #     key          = "ferry/dev.tfstate"
  #     region       = "eu-west-1"
  #     encrypt      = true
  #     use_lockfile = true
  #   }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = {
      project     = "ferry"
      environment = var.environment
      managed-by  = "terraform"
    }
  }
}
