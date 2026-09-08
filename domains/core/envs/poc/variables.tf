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

# Raised from the 29000 default because measured p95 here is 16 s and the slowest
# observed request hit 39.7 s (4096 output tokens), which the default would have
# cut off with a 504. Requires the L-E5AE38E3 quota increase to be APPROVED before
# applying -- API Gateway validates this against the account quota.
variable "integration_timeout_ms" {
  type    = number
  default = 300000
}
