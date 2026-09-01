# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

variable "region" {
  type = string
  validation {
    condition     = can(regex("^[a-z]{2}-[a-z]+-[0-9]$", var.region))
    error_message = "invalid region."
  }
}
# dist path passed by the env (path.module = the env folder), preserves the
# filename/filebase64sha256 strings and avoids a diff when migrating to tf/.
variable "dist_path" {
  type = string
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
variable "audit_bus" {
  description = "Override for the audit bus (Audit domain). Empty = derived."
  type        = string
  default     = null
}

variable "config_table_name" {
  description = "Override for the config table (Governance). Empty = derived."
  type        = string
  default     = null
}
variable "hints_table_name" {
  description = "Override for the Routing_Hints table (Observability). Empty = derived."
  type        = string
  default     = null
}
variable "min_hint_samples" {
  description = "Minimum sample per key to trust the E[tokens_out] hint. Production: 20. Low-traffic PoC: smaller, otherwise the hint is never used."
  type        = number
  default     = 3
  validation {
    condition     = var.min_hint_samples >= 1
    error_message = "min_hint_samples must be >= 1."
  }
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
# Cognito (Governance via SSM) — optional override. User pool ARN (not
# issuer/client_id — those were only for the keyadmin HTTP API's native JWT
# authorizer; it now uses COGNITO_USER_POOLS, which validates against the pool ARN).
variable "cognito_user_pool_arn" {
  type    = string
  default = null
}
# This deployment's org (Governance via SSM) — optional override. Empty = read from
# the Environment Contract; see the comment in locals.deployment_org (main.tf).
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

# API Gateway access logging needs an ACCOUNT-level CloudWatch role, which is a
# singleton per account+region. The core domain owns it so that exactly one stack
# manages it. Set to false when the account is already configured (or when another
# stack owns the setting), otherwise two stacks would overwrite each other.
variable "manage_apigw_account_settings" {
  description = "Create the account-level API Gateway CloudWatch Logs role. false = assume the account is already configured."
  type        = bool
  default     = true
}

# Browser origins allowed to call the Core APIs (CORS), comma-separated.
# Empty (default) = no Access-Control-Allow-Origin header is emitted, so a browser
# on any origin is blocked — deny by default. Server-to-server callers (the OpenAI
# SDK drop-in) are unaffected, since CORS only applies to browsers. Set this to the
# console origin (e.g. https://<console-dist>.cloudfront.net) to use the Playground.
variable "console_origin" {
  description = "Comma-separated list of browser origins allowed by CORS. Empty = deny all browser origins."
  type        = string
  default     = ""
}
