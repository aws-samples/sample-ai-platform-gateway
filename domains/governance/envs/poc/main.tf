# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Governance domain — poc environment (thin wrapper around the domains/governance/tf module).
# Isolated state (local backend). New environment = another envs/<env> folder with its
# tfvars + backend, reusing the same module (Req 5.2).
terraform {
  required_version = ">= 1.9.0, < 2.0.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = ">= 6.0.0, < 7.0.0" }
  }
  # Backend lives in backend.tf (remote state on S3).
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { Project = var.project, Environment = var.environment, ManagedBy = "terraform", Domain = "governance", "auto-delete" = "no" }
  }
}

variable "region" {
  type    = string
  default = "us-west-2"
}
variable "project" {
  type    = string
  default = "aiplat"
}
variable "environment" {
  type    = string
  default = "poc"
}
# No admin_token variable here: config-api is gated by the API Gateway
# COGNITO_USER_POOLS authorizer, and the module no longer injects ADMIN_TOKEN.

# Optional Environment Contract overrides (passed through to the module).
variable "cost_store_table" {
  type    = string
  default = null
}
variable "mfa_configuration" {
  description = "Cognito MFA enforcement: ON (required, default) or OPTIONAL."
  type        = string
  default     = "ON"
}
variable "seed_user_password" {
  description = "Initial password for the seeded platform_admin users. No default on purpose: any default committed here is published in the repo and would silently become the password of an account that can change budgets and read cost data. Set it via TF_VAR_seed_user_password."
  type        = string
  sensitive   = true
  validation {
    condition     = length(var.seed_user_password) >= 16
    error_message = "seed_user_password must be at least 16 characters."
  }
}

module "domain" {
  source      = "../../tf"
  project     = var.project
  environment = var.environment
  region      = var.region
  # path.module here = the env folder; reproduces the original strings.
  dist_path        = "${path.module}/../../dist"
  config_seed_path = "${path.module}/default_config.json"

  cost_store_table   = var.cost_store_table
  seed_user_password = var.seed_user_password

  # MFA on the control-plane pool: ON by default (see the module for why).
  mfa_configuration = var.mfa_configuration
}

output "config_table" { value = module.domain.config_table }
output "admin_api_endpoint" { value = module.domain.admin_api_endpoint }
output "cognito_user_pool_id" { value = module.domain.cognito_user_pool_id }
output "cognito_client_id" { value = module.domain.cognito_client_id }
output "cognito_issuer" { value = module.domain.cognito_issuer }
