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
  response_streaming = var.response_streaming

  # Optional AgentCore Gateway transport. Same forwarding rule as above: the region has to
  # travel with the flag, and it has to match the aws.agentcore provider's region or the
  # router will sign for one region and call another — which the service rejects as if the
  # credentials were wrong.
  agentcore_gateway_enabled = var.agentcore_gateway_enabled
  agentcore_gateway_region  = var.agentcore_gateway_region
}
