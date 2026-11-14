# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Help domain — poc environment (thin wrapper around the domains/help/tf module).
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
    tags = { Project = var.project, Environment = var.environment, ManagedBy = "terraform", Domain = "help", "auto-delete" = "no" }
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
# origins; set it to the console origin to read help content from the console.
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

output "help_api_endpoint" { value = module.domain.help_api_endpoint }

# XC-IAC-006: adopt the live auto-created Lambda log group into state. Import blocks
# are only valid in the ROOT module, so this targets the child module's resource.
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
  to       = module.domain.aws_cloudwatch_log_group.help_api
  id       = "/aws/lambda/aiplat-poc-help-api"
}
