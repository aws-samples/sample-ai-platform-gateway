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
