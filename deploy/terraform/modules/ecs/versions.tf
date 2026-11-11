terraform {
  required_version = ">= 1.11" # checked with 1.16 (make tf-check)

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.70, < 7.0"
    }
  }
}
