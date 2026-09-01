# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Governance domain — reusable MODULE (backend/provider live in the env wrapper).
terraform {
  required_version = ">= 1.9.0, < 2.0.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = ">= 6.0.0, < 7.0.0" }
  }
}

# Paths passed by the env (where path.module = the env folder), preserving the
# filename/filebase64sha256/file() strings and avoiding a diff when migrating to tf/.
variable "dist_path" {
  type = string
}
variable "config_seed_path" {
  type = string
}

variable "region" {
  type    = string
  default = "us-west-2"
}
variable "project" {
  type    = string
  default = "aiplat"
  validation {
    condition     = can(regex("^[a-z0-9]+$", var.project))
    error_message = "project must match ^[a-z0-9]+$ (no hyphens/uppercase) to guarantee collision-free names."
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

# The single org of this deployment (single-org). Governance writes all config
# under ORG#<deployment_org>#... — the Core needs the SAME constant to build the
# same key on read (see aws_ssm_parameter.deployment_org below and its use
# in core/tf/main.tf). Changing this value without migrating the items already
# written in DynamoDB orphans the existing config (the old org becomes unreadable).
variable "deployment_org" {
  type    = string
  default = "XYZ_ORG"
}

# The other domains' endpoints now live in the frontend stack (env.js).
# Governance no longer injects the site.

# There is deliberately no admin_token variable in this module: the gate for
# config-api is the API Gateway COGNITO_USER_POOLS authorizer. config-api's Go
# code read ADMIN_TOKEN but never compared it, so keeping the variable would
# mislead a reader into relaxing the authorizer believing a token gate exists.


# --- Social login (Hosted UI + IdPs) — GATED FOUNDATION ---
# Nothing social is created while the credentials are empty. To turn on a
# provider, register its OAuth app and fill in client_id/secret (in a tfvars).
# Only Free/Pro use social; Business+ goes through corporate SSO (see aiplat-security.md).
# console_url is the OAuth code flow callback, and it is produced by the FRONTEND — which
# applies AFTER governance (position 1 vs 4 in the Apply Order). Reading from the SSM
# Contract carelessly would create a cycle, and a literal default reintroduces exactly the
# live URL that the decoupling forbids (the `scripts/verify-decoupling.sh` guard
# caught this).
#
# Output: explicit override OR a read of the frontend's SSM Contract.
#
#   existing environment       → reads /<proj>/<env>/frontend/console_url (already published)
#   new environment bootstrap  → `-var console_url=…` on the first governance apply,
#                                and on subsequent ones SSM already answers
#
# Always reading (instead of only when social is enabled) is deliberate: conditioning on
# social would make the value DISAPPEAR from the pool configuration while no provider
# was enabled, erasing in production a callback URL that becomes necessary again
# the moment someone turns on the first IdP. Config that vanishes on its own is worse than
# config that requires an argument at bootstrap.
variable "console_url" {
  description = "Console URL (OAuth callback). null = read from the SSM Contract when social login is enabled."
  type        = string
  default     = null
}
data "aws_ssm_parameter" "console_url" {
  count = var.console_url == null ? 1 : 0
  name  = "${local.ssm_root}/frontend/console_url"
}
variable "google_client_id" {
  type    = string
  default = ""
}
variable "google_client_secret" {
  type      = string
  default   = ""
  sensitive = true
}
variable "facebook_client_id" {
  type    = string
  default = ""
}
variable "facebook_client_secret" {
  type      = string
  default   = ""
  sensitive = true
}
# Microsoft comes in as OIDC (Entra ID). issuer in the format
# https://login.microsoftonline.com/<tenant>/v2.0
variable "microsoft_client_id" {
  type    = string
  default = ""
}
variable "microsoft_client_secret" {
  type      = string
  default   = ""
  sensitive = true
}
variable "microsoft_issuer" {
  type    = string
  default = ""
}

data "aws_caller_identity" "current" {}

locals {
  name = "${var.project}-${var.environment}-gov"
  dist = var.dist_path

  # Root of the Environment Contract (SSM), scoped per environment.
  ssm_root = "/${var.project}/${var.environment}"

  # Identifiers of OTHER domains: derived from the convention (not literals),
  # with an optional per-variable override (Req 2/4.5).
  cost_store_table = coalesce(var.cost_store_table, "${var.project}-${var.environment}-obs-cost-store")
  # Core counters: read by partition (CREDIT#<org>#<provider>) to show the
  # estimated credit consumption. A contract read, never a call to the Lambda.
  limits_table = coalesce(var.limits_table, "${var.project}-${var.environment}-inf-limits")

  # Audit domain bus, by naming convention (same pattern as the
  # limits_table and the gov-config read by other domains). Convention instead of
  # an SSM data source so as not to create an ORDER dependency between the stacks: the
  # governance stack must be able to come up even if audit does not exist yet.
  audit_bus     = coalesce(var.audit_bus, "${var.project}-${var.environment}-aud-events")
  audit_bus_arn = "arn:aws:events:${var.region}:${data.aws_caller_identity.current.account_id}:event-bus/${local.audit_bus}"

  # Hosted UI prefix: DERIVED from the convention, not a literal. It must be unique
  # across all of AWS, so a literal in the module would prevent a second environment from applying.
  hosted_ui_domain_prefix = "${var.project}-${var.environment}-auth"

  # OAuth callback: explicit override > frontend's SSM Contract > empty (with no
  # social enabled, nobody consumes it). Conditional instead of `coalesce` because
  # coalesce rejects an empty string, and "" is precisely the valid case here.
  # `nonsensitive` because the console URL is public (it is in every client's
  # browser). Without it the value inherits the `sensitive` of the SSM parameter, Terraform
  # can no longer compare it and every `plan` starts showing a PHANTOM change
  # in callback_urls/logout_urls — a diff that trains the operator to ignore
  # plan, which is the most expensive habit there is.
  console_url = var.console_url != null ? var.console_url : try(nonsensitive(data.aws_ssm_parameter.console_url[0].value), "")

  # Enabled social providers (only those with client_id filled in). Empty =
  # none created, no button in the console. Turn on = fill in the tfvars.
  social_enabled = compact([
    var.google_client_id != "" ? "Google" : "",
    var.facebook_client_id != "" ? "Facebook" : "",
    var.microsoft_client_id != "" ? "Microsoft" : "",
  ])
}

# --- Config store (single item pk="global") ---
resource "aws_dynamodb_table" "config" {
  name         = "${local.name}-config"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  attribute {
    name = "pk"
    type = "S"
  }

  # PITR: this table holds the routing/behavior config every request depends on; a bad write must be recoverable.
  point_in_time_recovery { enabled = true }
}

# --- Audit store (control plane audit trail) ---
# Governance-OWNED store (data with its own lifecycle). pk=org, time-sortable
# sk → efficient Query by org without a Scan. TTL expires the old history
# (90 days) automatically. Never stores a password/token — only metadata.
resource "aws_dynamodb_table" "audit" {
  name         = "${local.name}-audit"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "org"
  range_key    = "sk"
  attribute {
    name = "org"
    type = "S"
  }
  attribute {
    name = "sk"
    type = "S"
  }
  ttl {
    attribute_name = "ttl"
    enabled        = true
  }

  # PITR: the control plane trail is evidence of who changed what; it has no other copy.
  point_in_time_recovery { enabled = true }
}

# Seed of the `global` scope: ONLY behavior defaults (auto_cheapest, cache_ttl).
# Do NOT seed models here: there is no global model catalog — each org brings
# its own (BYO). A `routing` at the global level would appear in every org and could not
# be removed by them (maps merge by key during inheritance).
resource "aws_dynamodb_table_item" "seed" {
  table_name = aws_dynamodb_table.config.name
  hash_key   = aws_dynamodb_table.config.hash_key
  item = jsonencode({
    pk     = { S = "global" }
    config = { S = file(var.config_seed_path) }
  })
  lifecycle {
    ignore_changes = [item]
  }
}

# --- config-api (Go) ---
resource "aws_iam_role" "config_api" {
  name = "${local.name}-config-api"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "lambda.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}

resource "aws_iam_role_policy" "config_api" {
  name = "config-api"
  role = aws_iam_role.config_api.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"], Resource = "arn:aws:logs:*:*:*" },
      { Effect = "Allow", Action = ["dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:DeleteItem"], Resource = aws_dynamodb_table.config.arn },
      # Credit: READ ONLY of the Core's consumption counter (least privilege —
      # Governance never writes to another domain's counter).
      { Effect = "Allow", Action = ["dynamodb:GetItem"], Resource = "arn:aws:dynamodb:*:*:table/${local.limits_table}" },
      # Platform shared secret (pooled resale) and BYO credential per org.
      { Effect = "Allow", Action = ["secretsmanager:CreateSecret", "secretsmanager:PutSecretValue", "secretsmanager:TagResource"], Resource = ["arn:aws:secretsmanager:*:*:secret:aiplat/gateway/*", "arn:aws:secretsmanager:*:*:secret:aiplat/org/*"] },
      # Read of the org's BYO credential ONLY to list the provider's models (the key never leaves for the client).
      { Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = "arn:aws:secretsmanager:*:*:secret:aiplat/org/*" },
      # Members & Access (RBAC): management of the org's users in OUR pool.
      { Effect = "Allow", Action = ["cognito-idp:ListUsers", "cognito-idp:AdminCreateUser", "cognito-idp:AdminDeleteUser", "cognito-idp:AdminGetUser", "cognito-idp:AdminUpdateUserAttributes", "cognito-idp:AdminSetUserPassword", "cognito-idp:AdminResetUserPassword", "cognito-idp:AdminEnableUser", "cognito-idp:AdminDisableUser"], Resource = aws_cognito_user_pool.cp.arn },

      # Control plane audit: writes and queries its own org's trail.
      { Effect = "Allow", Action = ["dynamodb:PutItem", "dynamodb:Query"], Resource = aws_dynamodb_table.audit.arn },
      # Audit event emission to the Audit domain. Only PutEvents, only on the
      # audit bus — this domain publishes about ITSELF and cannot write
      # to the other domain's store.
      { Effect = "Allow", Action = ["events:PutEvents"], Resource = local.audit_bus_arn },
      # BYO Bedrock: list models in the client's account via AssumeRole.
      { Effect = "Allow", Action = ["sts:AssumeRole"], Resource = "arn:aws:iam::*:role/AIPlatGatewayAccess*" },
      # List models in the platform account (pooled).
      { Effect = "Allow", Action = ["bedrock:ListFoundationModels"], Resource = "*" },
      # X-Ray requires Resource = "*": trace segments are not addressable
      # resources, so this is the documented AWS pattern for tracing.
      { Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" },
    ]
  })
}

resource "aws_lambda_function" "config_api" {
  function_name    = "${local.name}-config-api"
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  role             = aws_iam_role.config_api.arn
  filename         = "${local.dist}/config-api.zip"
  source_code_hash = filebase64sha256("${local.dist}/config-api.zip")
  timeout          = 15
  memory_size      = 128
  environment {
    variables = {
      CONFIG_TABLE = aws_dynamodb_table.config.name
      # No ADMIN_TOKEN here on purpose: the gate for this API is the API Gateway
      # COGNITO_USER_POOLS authorizer. config-api never compared the token, so
      # injecting one would suggest a second gate that does not exist.
      USER_POOL_ID = aws_cognito_user_pool.cp.id
      LIMITS_TABLE = local.limits_table
      AUDIT_TABLE  = aws_dynamodb_table.audit.name
      # Audit domain bus. Empty TURNS OFF emission without breaking
      # anything — it is what lets this domain come up before audit exists.
      AUDIT_BUS = local.audit_bus
      # Exact console origin (frontend's SSM Contract, the same local.console_url
      # already used in the Cognito callback). Go accepts a comma-separated
      # list (buildAllowedOrigins in cmd/config-api/main.go) — empty (bootstrap
      # with no frontend yet) turns off CORS entirely, a safe degradation: deny by
      # default instead of accepting an example domain that does not exist.
      CONSOLE_ORIGIN = local.console_url == null ? "" : trimsuffix(local.console_url, "/console.html")
    }
  }

  tracing_config { mode = "Active" }
}

# --- Admin API (REST API + native COGNITO_USER_POOLS authorizer) ---
# Switched from HTTP API to REST API: HTTP API does not support AWS WAF, resource
# policies, request validation, or private endpoints — REST API supports all of them.
# REST API has no native JWT authorizer, but it has COGNITO_USER_POOLS, which covers
# the same case without needing a custom Lambda authorizer.
resource "aws_api_gateway_rest_api" "admin" {
  name = "${local.name}-admin"
  endpoint_configuration {
    types = ["REGIONAL"]
  }
}

resource "aws_api_gateway_authorizer" "cognito" {
  name                             = "${local.name}-cognito"
  rest_api_id                      = aws_api_gateway_rest_api.admin.id
  type                             = "COGNITO_USER_POOLS"
  provider_arns                    = [aws_cognito_user_pool.cp.arn]
  identity_source                  = "method.request.header.Authorization"
  authorizer_result_ttl_in_seconds = 300
}

# A single {proxy+} resource dispatches ALL /admin/* routes to the same
# Lambda — path routing stays in the Go handler (existing path switch).
resource "aws_api_gateway_resource" "proxy" {
  rest_api_id = aws_api_gateway_rest_api.admin.id
  parent_id   = aws_api_gateway_rest_api.admin.root_resource_id
  path_part   = "{proxy+}"
}

resource "aws_api_gateway_method" "proxy_any" {
  rest_api_id   = aws_api_gateway_rest_api.admin.id
  resource_id   = aws_api_gateway_resource.proxy.id
  http_method   = "ANY"
  authorization = "COGNITO_USER_POOLS"
  authorizer_id = aws_api_gateway_authorizer.cognito.id
  request_parameters = {
    "method.request.path.proxy" = true
  }
}

resource "aws_api_gateway_integration" "proxy_any" {
  rest_api_id             = aws_api_gateway_rest_api.admin.id
  resource_id             = aws_api_gateway_resource.proxy.id
  http_method             = aws_api_gateway_method.proxy_any.http_method
  integration_http_method = "POST"
  type                    = "AWS_PROXY"
  uri                     = aws_lambda_function.config_api.invoke_arn
}

# OPTIONS without authorizer: a CORS preflight carries no token, so requiring
# Cognito here would keep the browser from even sending the real request.
resource "aws_api_gateway_method" "proxy_options" {
  rest_api_id   = aws_api_gateway_rest_api.admin.id
  resource_id   = aws_api_gateway_resource.proxy.id
  http_method   = "OPTIONS"
  authorization = "NONE"
  request_parameters = {
    "method.request.path.proxy" = true
  }
}

resource "aws_api_gateway_integration" "proxy_options" {
  rest_api_id             = aws_api_gateway_rest_api.admin.id
  resource_id             = aws_api_gateway_resource.proxy.id
  http_method             = aws_api_gateway_method.proxy_options.http_method
  integration_http_method = "POST"
  type                    = "AWS_PROXY"
  uri                     = aws_lambda_function.config_api.invoke_arn
}

resource "aws_api_gateway_deployment" "admin" {
  rest_api_id = aws_api_gateway_rest_api.admin.id
  triggers = {
    redeployment = sha1(jsonencode([
      aws_api_gateway_resource.proxy.id,
      aws_api_gateway_method.proxy_any.id,
      aws_api_gateway_integration.proxy_any.id,
      aws_api_gateway_method.proxy_options.id,
      aws_api_gateway_integration.proxy_options.id,
      aws_api_gateway_authorizer.cognito.id,
    ]))
  }
  lifecycle {
    create_before_destroy = true
  }
}

locals {
  # One JSON object per request: metadata only. No request or response body is
  # logged, so a prompt can never reach the access log.
  apigw_access_log_format = jsonencode({
    requestId      = "$context.requestId"
    ip             = "$context.identity.sourceIp"
    requestTime    = "$context.requestTime"
    httpMethod     = "$context.httpMethod"
    resourcePath   = "$context.resourcePath"
    status         = "$context.status"
    protocol       = "$context.protocol"
    responseLength = "$context.responseLength"
    latency        = "$context.responseLatency"
    userAgent      = "$context.identity.userAgent"
  })
}

resource "aws_cloudwatch_log_group" "apigw_admin" {
  name              = "/aws/apigateway/${local.name}-admin"
  retention_in_days = 90
}

resource "aws_api_gateway_stage" "admin" {
  rest_api_id   = aws_api_gateway_rest_api.admin.id
  deployment_id = aws_api_gateway_deployment.admin.id
  stage_name    = "prod"

  xray_tracing_enabled = true

  # Account-level API Gateway CloudWatch role is owned by the core domain (see manage_apigw_account_settings there).
  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.apigw_admin.arn
    format          = local.apigw_access_log_format
  }
}

resource "aws_lambda_permission" "admin" {
  statement_id  = "AllowAdminInvoke"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.config_api.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_api_gateway_rest_api.admin.execution_arn}/*/*"
}

# --- AWS WAF: the one protection HTTP API did not support and that drove this migration ---
resource "aws_wafv2_web_acl" "admin" {
  name = "${local.name}-waf"
  # No apostrophes: WAFv2 rejects them in description.
  description = "Standard OWASP protection and rate limit for the governance domain REST API."
  scope       = "REGIONAL"

  default_action {
    allow {}
  }

  rule {
    name     = "AWSManagedRulesCommonRuleSet"
    priority = 1
    override_action {
      none {}
    }
    statement {
      managed_rule_group_statement {
        name        = "AWSManagedRulesCommonRuleSet"
        vendor_name = "AWS"
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name}-common"
      sampled_requests_enabled   = true
    }
  }

  rule {
    name     = "AWSManagedRulesKnownBadInputsRuleSet"
    priority = 2
    override_action {
      none {}
    }
    statement {
      managed_rule_group_statement {
        name        = "AWSManagedRulesKnownBadInputsRuleSet"
        vendor_name = "AWS"
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name}-badinputs"
      sampled_requests_enabled   = true
    }
  }

  # A higher limit than the other domains: this API concentrates Members, Teams &
  # Apps, Limits & Budget and Audit — the entire administration console goes
  # through here, so the floor of legitimate traffic is higher.
  rule {
    name     = "RateLimitPerIP"
    priority = 3
    action {
      block {}
    }
    statement {
      rate_based_statement {
        limit              = 4000
        aggregate_key_type = "IP"
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name}-ratelimit"
      sampled_requests_enabled   = true
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = "${local.name}-waf"
    sampled_requests_enabled   = true
  }
}

resource "aws_wafv2_web_acl_association" "admin" {
  resource_arn = aws_api_gateway_stage.admin.arn
  web_acl_arn  = aws_wafv2_web_acl.admin.arn
}

# The site (landing + console) was extracted into its own domain `domains/frontend/`.
# Governance remains the owner of the control plane (Cognito) and the admin APIs; the front is
# a client of them and reads the endpoints via env.js in its own stack.

output "config_table" { value = aws_dynamodb_table.config.name }
output "admin_api_endpoint" { value = aws_api_gateway_stage.admin.invoke_url }

# --- Control plane identity (Cognito) ---
# Closes the hole: the static token was a global super-admin and allowed reading the cost of
# any org. Now each user has org_id in the JWT and the APIs scope by it.
# JWT validation is done by API Gateway (managed), not in our code.

resource "aws_cognito_user_pool" "cp" {
  name = "${local.name}-users"

  # Single-client deployment: no public sign-up. The admin invites members
  # (AdminCreateUser, via config-api); there is no /signup route nor creation of
  # a new org — this deployment's org is seeded once at bootstrap.
  auto_verified_attributes = ["email"]
  admin_create_user_config {
    allow_admin_create_user_only = true
  }

  # MFA (TOTP / software token). Defaults to ON: this pool guards the control
  # plane — the users who can change routing, budgets and provider credentials —
  # so a password alone is not an acceptable bar. With ON, Cognito issues the
  # MFA_SETUP challenge on first sign-in and SOFTWARE_TOKEN_MFA afterwards; the
  # console already implements both (see the enrolment flow in console.html).
  # OPTIONAL is still selectable for a throwaway demo, via TF_VAR_mfa_configuration.
  mfa_configuration = var.mfa_configuration
  software_token_mfa_configuration {
    enabled = true
  }

  # Password recovery via verified email (the console's ForgotPassword flow).
  account_recovery_setting {
    recovery_mechanism {
      name     = "verified_email"
      priority = 1
    }
  }

  # Sign-up verification email (code). {####} is the code placeholder.
  verification_message_template {
    default_email_option = "CONFIRM_WITH_CODE"
    email_subject        = "Your AIPlat confirmation code"
    email_message        = "Welcome to AIPlat. Your confirmation code is {####}"
  }

  # Injects the team scope (team/apps claims) into the JWT on every login. Foundation of
  # per-team enforcement (Slice B) — see aiplat-security.md.
  #
  # No post_confirmation: a single-client deployment does not create an org dynamically
  # (self-service signup was removed — allow_admin_create_user_only = true above).
  # The deployment's org is seeded once at bootstrap (config_seed_path).
  lambda_config {
    pre_token_generation = aws_lambda_function.pretoken.arn
  }

  password_policy {
    minimum_length    = 12
    require_lowercase = true
    require_numbers   = true
    require_symbols   = false
    require_uppercase = true
  }

  # org_id and role travel in the token and are the source of truth for scope.
  schema {
    name                     = "org_id"
    attribute_data_type      = "String"
    mutable                  = true
    developer_only_attribute = false
    string_attribute_constraints {
      min_length = 1
      max_length = 64
    }
  }
  schema {
    name                     = "role"
    attribute_data_type      = "String"
    mutable                  = true
    developer_only_attribute = false
    string_attribute_constraints {
      min_length = 1
      max_length = 32
    }
  }
}

# --- pretoken (Go): pre-token-generation trigger, injects team/apps claims ---
resource "aws_iam_role" "pretoken" {
  name = "${local.name}-pretoken"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "lambda.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}

resource "aws_iam_role_policy" "pretoken" {
  name = "pretoken"
  role = aws_iam_role.pretoken.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"], Resource = "arn:aws:logs:*:*:*" },
      # Least privilege: only reads the member record in the config.
      { Effect = "Allow", Action = ["dynamodb:GetItem"], Resource = aws_dynamodb_table.config.arn },
      # X-Ray requires Resource = "*": trace segments are not addressable
      # resources, so this is the documented AWS pattern for tracing.
      { Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" },
    ]
  })
}

resource "aws_lambda_function" "pretoken" {
  function_name    = "${local.name}-pretoken"
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  role             = aws_iam_role.pretoken.arn
  filename         = "${local.dist}/pretoken.zip"
  source_code_hash = filebase64sha256("${local.dist}/pretoken.zip")
  timeout          = 5
  memory_size      = 128
  environment {
    variables = {
      CONFIG_TABLE = aws_dynamodb_table.config.name
    }
  }

  tracing_config { mode = "Active" }
}

resource "aws_lambda_permission" "pretoken_cognito" {
  statement_id  = "AllowCognitoInvoke"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.pretoken.function_name
  principal     = "cognito-idp.amazonaws.com"
  source_arn    = aws_cognito_user_pool.cp.arn
}

# postconfirm (org-creation-on-signup trigger) removed: single-client deployment
# does not mint new orgs. The one org this deployment serves is seeded once via
# config_seed_path/aws_dynamodb_table_item.seed below, at bootstrap time.

# Hosted UI domain: needed for the OAuth code flow (social login). Creating the
# domain is harmless even without an IdP (Cognito serves its own hosted UI).
resource "aws_cognito_user_pool_domain" "hosted" {
  domain       = local.hosted_ui_domain_prefix
  user_pool_id = aws_cognito_user_pool.cp.id
}

# GATED IdPs: they only exist when the provider's client_id is filled in. Empty =
# count 0 = nothing created. Turn on a provider = fill in the tfvars and re-apply.
resource "aws_cognito_identity_provider" "google" {
  count         = var.google_client_id != "" ? 1 : 0
  user_pool_id  = aws_cognito_user_pool.cp.id
  provider_name = "Google"
  provider_type = "Google"
  provider_details = {
    client_id        = var.google_client_id
    client_secret    = var.google_client_secret
    authorize_scopes = "openid email profile"
  }
  attribute_mapping = { email = "email", name = "name", username = "sub" }
}

resource "aws_cognito_identity_provider" "facebook" {
  count         = var.facebook_client_id != "" ? 1 : 0
  user_pool_id  = aws_cognito_user_pool.cp.id
  provider_name = "Facebook"
  provider_type = "Facebook"
  provider_details = {
    client_id        = var.facebook_client_id
    client_secret    = var.facebook_client_secret
    authorize_scopes = "public_profile,email"
    api_version      = "v17.0"
  }
  attribute_mapping = { email = "email", name = "name", username = "id" }
}

resource "aws_cognito_identity_provider" "microsoft" {
  count         = var.microsoft_client_id != "" ? 1 : 0
  user_pool_id  = aws_cognito_user_pool.cp.id
  provider_name = "Microsoft"
  provider_type = "OIDC"
  provider_details = {
    client_id                 = var.microsoft_client_id
    client_secret             = var.microsoft_client_secret
    oidc_issuer               = var.microsoft_issuer
    authorize_scopes          = "openid email profile"
    attributes_request_method = "GET"
  }
  attribute_mapping = { email = "email", name = "name", username = "sub" }
}

resource "aws_cognito_user_pool_client" "console" {
  name         = "${local.name}-console"
  user_pool_id = aws_cognito_user_pool.cp.id

  generate_secret = false # SPA: no client secret
  explicit_auth_flows = [
    "ALLOW_USER_PASSWORD_AUTH",
    "ALLOW_REFRESH_TOKEN_AUTH",
  ]
  access_token_validity  = 8
  id_token_validity      = 8
  refresh_token_validity = 30
  token_validity_units {
    access_token  = "hours"
    id_token      = "hours"
    refresh_token = "days"
  }

  # OAuth code flow (social login via Hosted UI). Additive: email/password
  # login (USER_PASSWORD_AUTH above) keeps working in parallel.
  allowed_oauth_flows_user_pool_client = true
  allowed_oauth_flows                  = ["code"]
  allowed_oauth_scopes                 = ["openid", "email", "profile"]
  callback_urls                        = compact([local.console_url])
  logout_urls                          = compact([local.console_url])
  # COGNITO (email/password) always; social providers come in as enabled.
  supported_identity_providers = concat(["COGNITO"], local.social_enabled)

  # Ensures the IdPs exist before referencing them in the client.
  depends_on = [
    aws_cognito_identity_provider.google,
    aws_cognito_identity_provider.facebook,
    aws_cognito_identity_provider.microsoft,
  ]
}

# Platform user (super-admin: can cross orgs) and org user (scoped).
resource "aws_cognito_user" "platform" {
  user_pool_id = aws_cognito_user_pool.cp.id
  username     = "admin@aiplat.local"
  password     = var.seed_user_password
  attributes = {
    email          = "admin@aiplat.local"
    email_verified = true
    "org_id"       = "platform"
    "role"         = "platform_admin"
  }
}

resource "aws_cognito_user" "demo_org" {
  user_pool_id = aws_cognito_user_pool.cp.id
  username     = "admin@xyzorg.local"
  password     = var.seed_user_password
  attributes = {
    email          = "admin@xyzorg.local"
    email_verified = true
    # This deployment's single org. Every seeded user, config scope and demo
    # record uses this same org id — a deployment never has more than one.
    "org_id" = "XYZ_ORG"
    "role"   = "admin"
  }
}

variable "seed_user_password" {
  description = "Initial password for the seeded platform_admin users. No default on purpose: any default committed here is published in the repo and would silently become the password of an account that can change budgets and read cost data. Set it via TF_VAR_seed_user_password."
  type        = string
  sensitive   = true
  validation {
    condition     = length(var.seed_user_password) >= 16
    error_message = "seed_user_password must be at least 16 characters."
  }
}

# MFA enforcement on the control-plane user pool. ON (default) requires every
# member to enrol a TOTP factor; OPTIONAL leaves it to each member. "OFF" is
# deliberately not accepted: this pool can change budgets and read cost data.
variable "mfa_configuration" {
  description = "Cognito MFA enforcement: ON (required, default) or OPTIONAL."
  type        = string
  default     = "ON"
  validation {
    condition     = contains(["ON", "OPTIONAL"], var.mfa_configuration)
    error_message = "mfa_configuration must be ON or OPTIONAL (OFF is not supported for the control plane)."
  }
}

output "cognito_user_pool_id" { value = aws_cognito_user_pool.cp.id }
output "cognito_client_id" { value = aws_cognito_user_pool_client.console.id }
output "cognito_issuer" { value = "https://cognito-idp.${var.region}.amazonaws.com/${aws_cognito_user_pool.cp.id}" }

variable "audit_bus" {
  description = "Optional override for the Audit domain's audit bus. Empty = derived from the convention."
  type        = string
  default     = null
}

variable "limits_table" {
  description = "Optional override for the Core counters table. Empty = derived from the convention."
  type        = string
  default     = null
}

variable "cost_store_table" {
  description = "Optional override for the Cost_Store (obs). Empty = derived from the convention."
  type        = string
  default     = null
}

# --- Environment Contract (SSM): governance publishes its identifiers ---
# Only the NON-derivable ones (URLs and IDs generated by AWS). Table/bus names are
# derived from the convention in the consumers — they do not go to SSM.
resource "aws_ssm_parameter" "admin_api_url" {
  name  = "${local.ssm_root}/governance/admin_api_url"
  type  = "String"
  value = aws_api_gateway_stage.admin.invoke_url
}
# Full user pool ARN — needed for the COGNITO_USER_POOLS authorizer
# of the API Gateway REST API (aws_api_gateway_authorizer.provider_arns). The HTTP API's
# native JWT authorizer used issuer/audience; REST API uses the ARN.
resource "aws_ssm_parameter" "cognito_user_pool_arn" {
  name  = "${local.ssm_root}/governance/cognito_user_pool_arn"
  type  = "String"
  value = aws_cognito_user_pool.cp.arn
}
resource "aws_ssm_parameter" "cognito_user_pool_id" {
  name  = "${local.ssm_root}/governance/cognito_user_pool_id"
  type  = "String"
  value = aws_cognito_user_pool.cp.id
}
resource "aws_ssm_parameter" "cognito_client_id" {
  name  = "${local.ssm_root}/governance/cognito_client_id"
  type  = "String"
  value = aws_cognito_user_pool_client.console.id
}
resource "aws_ssm_parameter" "cognito_issuer" {
  name  = "${local.ssm_root}/governance/cognito_issuer"
  type  = "String"
  value = "https://cognito-idp.${var.region}.amazonaws.com/${aws_cognito_user_pool.cp.id}"
}

# Contract for the frontend: Hosted UI domain and enabled social providers.
resource "aws_ssm_parameter" "cognito_domain" {
  name  = "${local.ssm_root}/governance/cognito_domain"
  type  = "String"
  value = "https://${aws_cognito_user_pool_domain.hosted.domain}.auth.${var.region}.amazoncognito.com"
}
resource "aws_ssm_parameter" "deployment_org" {
  name  = "${local.ssm_root}/governance/deployment_org"
  type  = "String"
  value = var.deployment_org
}
resource "aws_ssm_parameter" "social_providers" {
  name = "${local.ssm_root}/governance/social_providers"
  type = "String"
  # Empty when no provider is enabled (the frontend shows no button).
  value = length(local.social_enabled) > 0 ? join(",", local.social_enabled) : "none"
}
