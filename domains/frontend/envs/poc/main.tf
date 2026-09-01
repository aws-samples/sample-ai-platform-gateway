# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Frontend domain — poc environment (thin wrapper around the domains/frontend/tf module).
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
    tags = { Project = var.project, Environment = var.environment, ManagedBy = "terraform", Domain = "frontend", "auto-delete" = "no" }
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
# Optional Environment Contract overrides (passed through to the module).
variable "admin_api_endpoint" {
  type    = string
  default = null
}
variable "cognito_client_id" {
  type    = string
  default = null
}
variable "gateway_endpoint" {
  type    = string
  default = null
}
variable "keyadmin_endpoint" {
  type    = string
  default = null
}
variable "usage_api_endpoint" {
  type    = string
  default = null
}
# Custom domain for the console. Both must be set together; the certificate has
# to be issued in us-east-1 (CloudFront requirement).
variable "domain_name" {
  type    = string
  default = null
}
variable "acm_certificate_arn" {
  type    = string
  default = null
}

module "domain" {
  source      = "../../tf"
  project     = var.project
  environment = var.environment
  region      = var.region
  # path.module here = the env folder; reproduces the original source/filemd5 string.
  site_path = "${path.module}/../../site"

  admin_api_endpoint = var.admin_api_endpoint
  cognito_client_id  = var.cognito_client_id
  gateway_endpoint   = var.gateway_endpoint
  keyadmin_endpoint  = var.keyadmin_endpoint
  usage_api_endpoint = var.usage_api_endpoint

  # Optional custom domain. Supplying both raises the TLS floor to TLSv1.2_2021;
  # leaving them null keeps the CloudFront default certificate (pinned to TLSv1).
  domain_name         = var.domain_name
  acm_certificate_arn = var.acm_certificate_arn
}

# State-ONLY re-addressing (Terraform 1.9 moved): existing resources move
# to live under module.domain.* without being recreated. plan should show only "moved".
moved {
  from = aws_s3_bucket.site
  to   = module.domain.aws_s3_bucket.site
}
moved {
  from = aws_s3_bucket_public_access_block.site
  to   = module.domain.aws_s3_bucket_public_access_block.site
}
moved {
  from = aws_cloudfront_origin_access_control.site
  to   = module.domain.aws_cloudfront_origin_access_control.site
}
moved {
  from = aws_cloudfront_distribution.site
  to   = module.domain.aws_cloudfront_distribution.site
}
moved {
  from = aws_s3_bucket_policy.site
  to   = module.domain.aws_s3_bucket_policy.site
}
moved {
  from = aws_s3_object.console
  to   = module.domain.aws_s3_object.console
}
moved {
  from = aws_s3_object.env
  to   = module.domain.aws_s3_object.env
}
moved {
  from = aws_ssm_parameter.console_url
  to   = module.domain.aws_ssm_parameter.console_url
}

output "site_bucket" { value = module.domain.site_bucket }
output "console_url" { value = module.domain.console_url }
