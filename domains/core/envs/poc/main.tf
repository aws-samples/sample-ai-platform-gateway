# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Core domain — poc environment (thin wrapper around the domains/core/tf module).
# backend.tf, providers.tf, versions.tf and variables.tf stay in the env.
module "domain" {
  source = "../../tf"
  providers = {
    aws           = aws
    aws.agentcore = aws.agentcore
  }
  project     = var.project
  environment = var.environment
  region      = var.region
  # path.module here = the env folder; reproduces the original zip strings.
  dist_path = "${path.module}/../../dist"

  config_table_name = var.config_table_name
  usage_queue_arn   = var.usage_queue_arn
  usage_queue_url   = var.usage_queue_url
  secret_prefix_arn = var.secret_prefix_arn

  model_routing_fallback = var.model_routing_fallback
  pricing_fallback       = var.pricing_fallback

  cognito_user_pool_arn = var.cognito_user_pool_arn
  deployment_org        = var.deployment_org

  # CORS allowlist for browser callers. Empty = deny all browser origins.
  console_origin = var.console_origin

  integration_timeout_ms = var.integration_timeout_ms

  # Must be forwarded explicitly. This wrapper passes variables one by one, so a
  # variable declared in the module but not listed here silently takes the module
  # default — which is how the first attempt at enabling streaming failed: the module
  # stayed BUFFERED while the timeout was raised to 900000, and the provider rejected
  # the pair. The rejection was correct and cost nothing, but the cause was here.
  response_streaming             = var.response_streaming
  router_provisioned_concurrency = var.router_provisioned_concurrency

  # Optional AgentCore Gateway transport. Same forwarding rule as above: the region has to
  # travel with the flag, and it has to match the aws.agentcore provider's region or the
  # router will sign for one region and call another — which the service rejects as if the
  # credentials were wrong.
  agentcore_gateway_enabled = var.agentcore_gateway_enabled
  agentcore_gateway_region  = var.agentcore_gateway_region

  # Optional native InvokeModel target (Nova, Llama, versioned ids). Same forwarding rule:
  # a variable declared in the module but not listed here silently takes the module default.
  agentcore_invoke_enabled = var.agentcore_invoke_enabled

  # Optional Phase 2 model-fallback rule (CloudFormation). Same forwarding rule. The rule's
  # match/primary/fallback models take the module defaults (nova-micro primary, nova-lite
  # fallback) unless overridden.
  agentcore_rules_enabled = var.agentcore_rules_enabled

  # Optional Phase 4 usage-reconciler (gateway routing-log -> Cost_Store GATEWAY facts).
  # Same forwarding rule.
  agentcore_reconciler_enabled = var.agentcore_reconciler_enabled
}

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
  to       = module.domain.aws_cloudwatch_log_group.router_logs
  id       = "/aws/lambda/aiplat-poc-inf-router"
}
import {
  for_each = var.adopt_log_groups ? [1] : []
  to       = module.domain.aws_cloudwatch_log_group.keyadmin_logs
  id       = "/aws/lambda/aiplat-poc-inf-keyadmin"
}
