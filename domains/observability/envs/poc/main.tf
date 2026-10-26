# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Observability domain — poc environment (thin wrapper around the domains/observability/tf module).
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
    tags = { Project = var.project, Environment = var.environment, ManagedBy = "terraform", Domain = "observability", "auto-delete" = "no" }
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
# No admin_token variable here: the usage API is gated by the API Gateway
# COGNITO_USER_POOLS authorizer, and the module no longer injects ADMIN_TOKEN.
variable "cognito_user_pool_arn" {
  type    = string
  default = null
}

# Browser origins allowed by CORS (comma-separated). Empty = deny all browser
# origins; set it to the console origin to read usage/cost from the console.
variable "console_origin" {
  type    = string
  default = ""
}

module "domain" {
  source      = "../../tf"
  project     = var.project
  environment = var.environment
  region      = var.region
  dist_path   = "${path.module}/../../dist"

  cognito_user_pool_arn = var.cognito_user_pool_arn
  console_origin        = var.console_origin
}

output "usage_queue_url" { value = module.domain.usage_queue_url }
output "usage_queue_arn" { value = module.domain.usage_queue_arn }
output "cost_store_table" { value = module.domain.cost_store_table }
output "event_bus_name" { value = module.domain.event_bus_name }
output "usage_api_endpoint" { value = module.domain.usage_api_endpoint }
