# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Audit domain — poc environment (thin wrapper around the domains/audit/tf module).
# Isolated state (local backend). New environment = another envs/<env> folder.
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
    tags = { Project = var.project, Environment = var.environment, ManagedBy = "terraform", Domain = "audit", "auto-delete" = "no" }
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

# Browser origins allowed by CORS (comma-separated). Empty = deny all browser
# origins; set it to the console origin to read the audit trail from the console.
variable "console_origin" {
  type    = string
  default = ""
}

module "domain" {
  source         = "../../tf"
  project        = var.project
  environment    = var.environment
  region         = var.region
  dist_path      = "${path.module}/../../dist"
  console_origin = var.console_origin
}

output "audit_api_endpoint" { value = module.domain.audit_api_endpoint }
output "audit_bus_name" { value = module.domain.audit_bus_name }
output "audit_bus_arn" { value = module.domain.audit_bus_arn }
output "trail_table" { value = module.domain.trail_table }
output "ingest_dlq_url" { value = module.domain.ingest_dlq_url }
output "archive_bucket" { value = module.domain.archive_bucket }
