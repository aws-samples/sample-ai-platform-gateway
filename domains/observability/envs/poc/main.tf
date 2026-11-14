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

# OBS-FUN-003: this deployment's single org (same value the core uses). The notifier and
# hints-publisher evaluate/publish only for it.
variable "deployment_org" {
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
  deployment_org        = var.deployment_org
}

output "usage_queue_url" { value = module.domain.usage_queue_url }
output "usage_queue_arn" { value = module.domain.usage_queue_arn }
output "cost_store_table" { value = module.domain.cost_store_table }
output "usage_api_endpoint" { value = module.domain.usage_api_endpoint }

# XC-IAC-006: adopt the live auto-created Lambda log groups into state. Import blocks
# are only valid in the ROOT module, so they target the child module's resources.
# Plan shows import + retention update, not create (which fails
# ResourceAlreadyExistsException). One-time bootstrap on an EXISTING stack; remove for
# a greenfield deploy. See README "Quick start".
import {
  to = module.domain.aws_cloudwatch_log_group.usage_writer
  id = "/aws/lambda/aiplat-poc-obs-usage-writer"
}
import {
  to = module.domain.aws_cloudwatch_log_group.usage_api
  id = "/aws/lambda/aiplat-poc-obs-usage-api"
}
import {
  to = module.domain.aws_cloudwatch_log_group.alert_notifier
  id = "/aws/lambda/aiplat-poc-obs-alert-notifier"
}
import {
  to = module.domain.aws_cloudwatch_log_group.hints_publisher
  id = "/aws/lambda/aiplat-poc-obs-hints-publisher"
}
