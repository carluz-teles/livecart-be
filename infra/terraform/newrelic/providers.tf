terraform {
  required_version = ">= 1.0"

  required_providers {
    newrelic = {
      source  = "newrelic/newrelic"
      version = "~> 3.96"
    }
  }

  # Uncomment to use S3 backend for state management (mirrors infra/terraform/main.tf)
  # backend "s3" {
  #   bucket  = "livecart-terraform-state"
  #   key     = "staging/newrelic/terraform.tfstate"
  #   region  = "us-east-1"
  #   encrypt = true
  # }
}

provider "newrelic" {
  account_id = var.newrelic_account_id
  api_key    = var.newrelic_api_key
  region     = "US"
}
