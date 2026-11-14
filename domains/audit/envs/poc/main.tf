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

# XC-IAC-006: adopt the live auto-created Lambda log groups into state. Import blocks
# are only valid in the ROOT module, so they target the child module's resources.
# Plan shows import + retention update, not create (which fails
# ResourceAlreadyExistsException). Guarded by adopt_log_groups (XC-IAC-007).
# XC-IAC-007: adopting the live log groups is OPT-IN. An existing deployment created these
# groups via Lambda auto-creation before they were managed, so the first apply must IMPORT
# them (set adopt_log_groups=true); a greenfield account where they do not exist yet applies
# with the default false and CREATES them. The old unguarded import blocks made `terraform
# plan` fail with "Cannot import non-existent remote object" on every fresh account. See the
# README "Existing deployment: first apply" step.
variable "adopt_log_groups" {
  type    = bool
  default = false
}

import {
  for_each = var.adopt_log_groups ? [1] : []
  to       = module.domain.aws_cloudwatch_log_group.archiver
  id       = "/aws/lambda/aiplat-poc-aud-archiver"
}
import {
  for_each = var.adopt_log_groups ? [1] : []
  to       = module.domain.aws_cloudwatch_log_group.writer_logs
  id       = "/aws/lambda/aiplat-poc-aud-writer"
}
import {
  for_each = var.adopt_log_groups ? [1] : []
  to       = module.domain.aws_cloudwatch_log_group.api_logs
  id       = "/aws/lambda/aiplat-poc-aud-api"
}
