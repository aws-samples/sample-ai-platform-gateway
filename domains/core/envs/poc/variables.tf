# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

variable "region" {
  type = string
  validation {
    condition     = can(regex("^[a-z]{2}-[a-z]+-[0-9]$", var.region))
    error_message = "invalid region."
  }
}
variable "project" {
  type    = string
  default = "aiplat"
  validation {
    condition     = can(regex("^[a-z0-9]+$", var.project))
    error_message = "project must match ^[a-z0-9]+$ (no hyphens/uppercase)."
  }
}
variable "environment" {
  type    = string
  default = "poc"
  validation {
    condition     = can(regex("^[a-z0-9]+$", var.environment))
    error_message = "environment must match ^[a-z0-9]+$ (no hyphens/uppercase)."
  }
}

# --- Contracts consumed from other domains: optional overrides (Req 4.5) ---
# Empty = derived from the naming convention (see locals in main.tf).
variable "config_table_name" {
  description = "Override for the config table (Governance). Empty = derived."
  type        = string
  default     = null
}
variable "usage_queue_arn" {
  description = "Override for the Usage_Records queue ARN (Observability). Empty = derived."
  type        = string
  default     = null
}
variable "usage_queue_url" {
  description = "Override for the Usage_Records queue URL (Observability). Empty = derived."
  type        = string
  default     = null
}
# Cognito (Governance via SSM) — optional override.
variable "cognito_user_pool_arn" {
  type    = string
  default = null
}
variable "deployment_org" {
  type    = string
  default = null
}
variable "secret_prefix_arn" {
  description = "ARN prefix for the provider secrets (Models domain)."
  type        = string
  default     = "arn:aws:secretsmanager:*:*:secret:aiplat/gateway/*"
}

# --- Routing/pricing fallback (environment defaults) ---
variable "model_routing_fallback" {
  type    = string
  default = "{}"
}
variable "pricing_fallback" {
  type    = string
  default = "{}"
}

# Browser origins allowed by CORS (comma-separated). Empty = deny all browser
# origins; set it to the console origin to use the Playground from the console.
variable "console_origin" {
  type    = string
  default = ""
}

# No admin_token variable here: the keyadmin API is gated by the API Gateway
# COGNITO_USER_POOLS authorizer, and the module no longer injects ADMIN_TOKEN.

# Same as the module default. Kept explicit here because this is the value the demo is
# sized against: measured p95 is 16 s and the slowest observed request took 39.7 s (4096
# output tokens), all of which the old 29000 cut off with a 504.
#
# Reachable only because the gateway route is a STREAM integration — the L-E5AE38E3
# account quota bounds BUFFERED integrations, and this one is not, so the pending quota
# increase is no longer on the critical path.
variable "integration_timeout_ms" {
  type    = number
  default = 300000
}

# Enable API Gateway response streaming on the gateway route.
#
# One flag drives three settings that must agree: the integration's transfer mode, the
# integration URI, and the router's AIPLAT_RESPONSE_MODE. A mismatch does not raise an
# error — API Gateway answers with the right status code and an empty body — so they are
# deliberately not separately settable.
variable "response_streaming" {
  type    = bool
  default = true
}

variable "agentcore_gateway_enabled" {
  type = bool
  # This poc environment DOES run the AgentCore Gateway (the demo uses provider
  # "bedrock_gateway" routes), so the env opts in here. The reusable module in ../../tf
  # keeps its own default at false — turning the gateway on is this environment's
  # deliberate choice, not the module's. Kept in tracked config on purpose: the gateway
  # used to be enabled only via an out-of-band -var/TF_VAR at apply time, so any plain
  # `terraform apply` planned to DESTROY the live gateway (count -> 0). Declaring it true
  # here makes the tracked state match what is deployed.
  default     = true
  description = "Create the optional AgentCore Gateway and enable provider bedrock_gateway routes. This poc env opts in; the module default stays false."
}

variable "agentcore_gateway_region" {
  type        = string
  default     = "us-east-1"
  description = <<-EOT
    Region for the AgentCore Gateway. Must stay in sync with the aws.agentcore provider in
    providers.tf — they are two places that read the same variable precisely so they cannot
    disagree. us-east-1 by default because that is where the gateway serves a full Anthropic
    ladder; us-west-2 serves only claude-haiku-4-5.
  EOT
}
