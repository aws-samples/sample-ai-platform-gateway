# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Observability domain — reusable MODULE (backend/provider live in the env wrapper).
terraform {
  required_version = ">= 1.9.0, < 2.0.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = ">= 6.0.0, < 7.0.0" }
  }
}

variable "dist_path" {
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

# PoC: static admin token (known technical debt — evolve to Cognito/SSO).
variable "admin_token" {
  description = "Admin token for the break-glass path. No default on purpose: a default would be applied silently and this value is published in the repo. Set it via TF_VAR_admin_token."
  type        = string
  sensitive   = true
  validation {
    condition     = length(var.admin_token) >= 32
    error_message = "admin_token must be at least 32 characters."
  }
}

# Optional Environment Contract override (Req 4.5). Empty = read from governance's SSM.
variable "cognito_user_pool_arn" {
  type    = string
  default = null
}

# Browser origins allowed to call the Usage API (CORS), comma-separated.
# Empty (default) = no Access-Control-Allow-Origin header is emitted, so a browser
# on any origin is blocked — deny by default. Server-to-server callers are
# unaffected, since CORS only applies to browsers. Set this to the console origin
# (e.g. https://<console-dist>.cloudfront.net) to read usage/cost from the console.
variable "console_origin" {
  description = "Comma-separated list of browser origins allowed by CORS. Empty = deny all browser origins."
  type        = string
  default     = ""
}

locals {
  name     = "${var.project}-${var.environment}-obs"
  dist     = var.dist_path
  ssm_root = "/${var.project}/${var.environment}"

  # REST API needs the user pool ARN (COGNITO_USER_POOLS authorizer), not the
  # issuer/audience (those were only for the HTTP API's native JWT authorizer).
  cognito_user_pool_arn = coalesce(var.cognito_user_pool_arn, try(nonsensitive(data.aws_ssm_parameter.cognito_user_pool_arn[0].value), null))
}

data "aws_ssm_parameter" "cognito_user_pool_arn" {
  count = var.cognito_user_pool_arn == null ? 1 : 0
  name  = "${local.ssm_root}/governance/cognito_user_pool_arn"
}

# --- Cost_Store ---
resource "aws_dynamodb_table" "cost_store" {
  name         = "${local.name}-cost-store"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  range_key    = "sk"
  attribute {
    name = "pk"
    type = "S"
  }
  attribute {
    name = "sk"
    type = "S"
  }
  attribute {
    name = "gsi1pk"
    type = "S"
  }
  global_secondary_index {
    name            = "gsi1"
    hash_key        = "gsi1pk"
    range_key       = "sk"
    projection_type = "ALL"
  }

  # TTL: without this the table grew forever — no writer computed
  # expires_at for USAGE items (only ALERTSTATE#/ALERTLOG#, written by the
  # alert-notifier, already had it). usage-writer now sets expires_at = ts + 120
  # days (const hotRetention); the UI never queries usage/summary beyond 90
  # days nor usage/records beyond 30, so 120 gives margin without retaining history
  # that nobody reads through the synchronous API.
  ttl {
    attribute_name = "expires_at"
    enabled        = true
  }

  # PITR: this table is the only record of tenant spend; losing it loses billing evidence.
  point_in_time_recovery { enabled = true }
}

# --- Usage_Records queue + DLQ ---
# Encryption at rest with SSE-SQS (AWS-managed key): usage records carry tenant,
# model and cost metadata, so they must not sit unencrypted in the queue.
resource "aws_sqs_queue" "dlq" {
  name                      = "${local.name}-usage-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true
}
resource "aws_sqs_queue" "usage" {
  name                       = "${local.name}-usage"
  visibility_timeout_seconds = 60
  redrive_policy             = jsonencode({ deadLetterTargetArn = aws_sqs_queue.dlq.arn, maxReceiveCount = 5 })
  sqs_managed_sse_enabled    = true
}

# --- FinOps event bus ---
resource "aws_cloudwatch_event_bus" "finops" {
  name = "${local.name}-finops"
}

# --- usage-writer (Go) ---
resource "aws_iam_role" "writer" {
  name = "${local.name}-writer"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "lambda.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}
resource "aws_iam_role_policy" "writer" {
  name = "writer"
  role = aws_iam_role.writer.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"], Resource = "arn:aws:logs:*:*:*" },
      { Effect = "Allow", Action = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes"], Resource = aws_sqs_queue.usage.arn },
      { Effect = "Allow", Action = ["dynamodb:PutItem"], Resource = aws_dynamodb_table.cost_store.arn },
      # X-Ray requires Resource = "*": trace segments are not addressable
      # resources, so this is the documented AWS pattern for tracing.
      { Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" },
    ]
  })
}
resource "aws_lambda_function" "writer" {
  function_name    = "${local.name}-usage-writer"
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  role             = aws_iam_role.writer.arn
  filename         = "${local.dist}/usage-writer.zip"
  source_code_hash = filebase64sha256("${local.dist}/usage-writer.zip")
  timeout          = 30
  memory_size      = 128
  environment {
    variables = { COST_STORE_TABLE = aws_dynamodb_table.cost_store.name }
  }

  tracing_config { mode = "Active" }
}
resource "aws_lambda_event_source_mapping" "usage" {
  event_source_arn = aws_sqs_queue.usage.arn
  function_name    = aws_lambda_function.writer.arn
  batch_size       = 10
}

# --- usage-api (Go): unified cost correlation layer (read/aggregation) ---
resource "aws_iam_role" "usage_api" {
  name = "${local.name}-usage-api"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "lambda.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}
resource "aws_iam_role_policy" "usage_api" {
  name = "usage-api"
  role = aws_iam_role.usage_api.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"], Resource = "arn:aws:logs:*:*:*" },
      { Effect = "Allow", Action = ["dynamodb:Query"], Resource = [aws_dynamodb_table.cost_store.arn, "${aws_dynamodb_table.cost_store.arn}/index/*"] },
      # X-Ray requires Resource = "*": trace segments are not addressable
      # resources, so this is the documented AWS pattern for tracing.
      { Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" },
    ]
  })
}
resource "aws_lambda_function" "usage_api" {
  function_name    = "${local.name}-usage-api"
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  role             = aws_iam_role.usage_api.arn
  filename         = "${local.dist}/usage-api.zip"
  source_code_hash = filebase64sha256("${local.dist}/usage-api.zip")
  timeout          = 30
  memory_size      = 256
  environment {
    variables = {
      COST_STORE_TABLE = aws_dynamodb_table.cost_store.name
      ADMIN_TOKEN      = var.admin_token
      CONSOLE_ORIGIN   = var.console_origin
    }
  }

  tracing_config { mode = "Active" }
}

# --- REST API + native COGNITO_USER_POOLS authorizer ---
# Switched from HTTP API to REST API: HTTP API does not support AWS WAF, resource
# policies, request validation, or private endpoints — REST API supports all of them.
# REST API has no native JWT authorizer, but it has COGNITO_USER_POOLS, which covers
# the same case without needing a custom Lambda authorizer.
resource "aws_api_gateway_rest_api" "usage" {
  name = "${local.name}-usage-api"
  endpoint_configuration {
    types = ["REGIONAL"]
  }
}

resource "aws_api_gateway_authorizer" "cognito" {
  name                             = "${local.name}-cognito"
  rest_api_id                      = aws_api_gateway_rest_api.usage.id
  type                             = "COGNITO_USER_POOLS"
  provider_arns                    = [local.cognito_user_pool_arn]
  identity_source                  = "method.request.header.Authorization"
  authorizer_result_ttl_in_seconds = 300
}

# A single {proxy+} resource dispatches /usage/summary, /usage/records and
# /usage/alerts to the same Lambda — path routing stays in the Go handler.
resource "aws_api_gateway_resource" "proxy" {
  rest_api_id = aws_api_gateway_rest_api.usage.id
  parent_id   = aws_api_gateway_rest_api.usage.root_resource_id
  path_part   = "{proxy+}"
}

resource "aws_api_gateway_method" "proxy_any" {
  rest_api_id   = aws_api_gateway_rest_api.usage.id
  resource_id   = aws_api_gateway_resource.proxy.id
  http_method   = "ANY"
  authorization = "COGNITO_USER_POOLS"
  authorizer_id = aws_api_gateway_authorizer.cognito.id
  request_parameters = {
    "method.request.path.proxy" = true
  }
}

resource "aws_api_gateway_integration" "proxy_any" {
  rest_api_id             = aws_api_gateway_rest_api.usage.id
  resource_id             = aws_api_gateway_resource.proxy.id
  http_method             = aws_api_gateway_method.proxy_any.http_method
  integration_http_method = "POST"
  type                    = "AWS_PROXY"
  uri                     = aws_lambda_function.usage_api.invoke_arn
}

# OPTIONS without authorizer: a CORS preflight carries no token, so requiring
# Cognito here would keep the browser from even sending the real request.
resource "aws_api_gateway_method" "proxy_options" {
  rest_api_id   = aws_api_gateway_rest_api.usage.id
  resource_id   = aws_api_gateway_resource.proxy.id
  http_method   = "OPTIONS"
  authorization = "NONE"
  request_parameters = {
    "method.request.path.proxy" = true
  }
}

resource "aws_api_gateway_integration" "proxy_options" {
  rest_api_id             = aws_api_gateway_rest_api.usage.id
  resource_id             = aws_api_gateway_resource.proxy.id
  http_method             = aws_api_gateway_method.proxy_options.http_method
  integration_http_method = "POST"
  type                    = "AWS_PROXY"
  uri                     = aws_lambda_function.usage_api.invoke_arn
}

resource "aws_api_gateway_deployment" "usage" {
  rest_api_id = aws_api_gateway_rest_api.usage.id
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

resource "aws_cloudwatch_log_group" "apigw_usage" {
  name              = "/aws/apigateway/${local.name}-usage"
  retention_in_days = 90
}

resource "aws_api_gateway_stage" "usage" {
  rest_api_id   = aws_api_gateway_rest_api.usage.id
  deployment_id = aws_api_gateway_deployment.usage.id
  stage_name    = "prod"

  xray_tracing_enabled = true

  # Account-level API Gateway CloudWatch role is owned by the core domain (see manage_apigw_account_settings there).
  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.apigw_usage.arn
    format          = local.apigw_access_log_format
  }
}

resource "aws_lambda_permission" "usage" {
  statement_id  = "AllowUsageApi"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.usage_api.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_api_gateway_rest_api.usage.execution_arn}/*/*"
}

# --- AWS WAF: the one protection HTTP API did not support and that drove this migration ---
resource "aws_wafv2_web_acl" "usage" {
  name = "${local.name}-waf"
  # No apostrophes: WAFv2 rejects them in description.
  description = "Standard OWASP protection and rate limit for the observability domain REST API."
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

  rule {
    name     = "RateLimitPerIP"
    priority = 3
    action {
      block {}
    }
    statement {
      rate_based_statement {
        limit              = 2000
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

resource "aws_wafv2_web_acl_association" "usage" {
  resource_arn = aws_api_gateway_stage.usage.arn
  web_acl_arn  = aws_wafv2_web_acl.usage.arn
}

# --- alert-notifier (Go): SCHEDULED alert evaluator + webhook delivery ---
# Reads the gov-config (contract) and the Cost_Store; POSTs to the org's webhook when it fires.
# The gov-config name comes by convention (${project}-${environment}-gov-config).
resource "aws_iam_role" "notifier" {
  name = "${local.name}-notifier"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "lambda.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}
resource "aws_iam_role_policy" "notifier" {
  name = "notifier"
  role = aws_iam_role.notifier.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"], Resource = "arn:aws:logs:*:*:*" },
      # Query on the Cost_Store (metrics) + PutItem for the cooldown marker and
      # for the firing history (ALERTLOG#<org> partition).
      { Effect = "Allow", Action = ["dynamodb:Query", "dynamodb:PutItem"], Resource = aws_dynamodb_table.cost_store.arn },
      # Read of the gov-config as a CONTRACT (same pattern as the Core). Least privilege: only the governance table.
      { Effect = "Allow", Action = ["dynamodb:Scan", "dynamodb:GetItem"], Resource = "arn:aws:dynamodb:${var.region}:${data.aws_caller_identity.current.account_id}:table/${var.project}-${var.environment}-gov-config" },
      # X-Ray requires Resource = "*": trace segments are not addressable
      # resources, so this is the documented AWS pattern for tracing.
      { Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" },
    ]
  })
}
resource "aws_lambda_function" "notifier" {
  function_name    = "${local.name}-alert-notifier"
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  role             = aws_iam_role.notifier.arn
  filename         = "${local.dist}/alert-notifier.zip"
  source_code_hash = filebase64sha256("${local.dist}/alert-notifier.zip")
  timeout          = 60
  memory_size      = 256
  environment {
    variables = {
      COST_STORE_TABLE = aws_dynamodb_table.cost_store.name
      CONFIG_TABLE     = "${var.project}-${var.environment}-gov-config"
    }
  }

  tracing_config { mode = "Active" }
}

# Schedule: evaluates every 15 minutes (the per-rule daily cooldown avoids spam).
resource "aws_cloudwatch_event_rule" "notifier_schedule" {
  name                = "${local.name}-alert-schedule"
  schedule_expression = "rate(15 minutes)"
}
resource "aws_cloudwatch_event_target" "notifier" {
  rule = aws_cloudwatch_event_rule.notifier_schedule.name
  arn  = aws_lambda_function.notifier.arn
}
resource "aws_lambda_permission" "notifier_schedule" {
  statement_id  = "AllowEventBridgeSchedule"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.notifier.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.notifier_schedule.arn
}

# --- Routing_Hints: asynchronous contract Observability → Core -----------------
#
# The Core needs the historical median of output tokens per (org, feature, model)
# to estimate cost, and the unavailability signal per model/provider. That data
# is born from the Cost_Store, which belongs to THIS domain — and the Core cannot call the usage-api
# (golden rule: never a synchronous call between domains).
#
# Solution: we publish an ARTIFACT that the Core reads as a contract, just like the
# `gov-config` pattern. A composite key so the Core does a GetItem for the current
# request's feature instead of a Query bringing everything:
#
#   pk = ORG#<org>            ← the partition carries the org (structural isolation)
#   sk = HINTS#v1#<feature>   ← or HINTS#v1#* for the org's aggregate
resource "aws_dynamodb_table" "routing_hints" {
  name         = "${local.name}-routing-hints"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  range_key    = "sk"
  attribute {
    name = "pk"
    type = "S"
  }
  attribute {
    name = "sk"
    type = "S"
  }
  # TTL: a hint that stopped being published DISAPPEARS instead of aging in
  # silence serving a stale prediction.
  ttl {
    attribute_name = "expires_at"
    enabled        = true
  }

  # PITR: cheap safety net for the Core's routing contract while the publisher rebuilds it.
  point_in_time_recovery { enabled = true }
}

# --- hints-publisher (Go): aggregates the Cost_Store and publishes the artifact -----------
resource "aws_iam_role" "hints" {
  name = "${local.name}-hints"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "lambda.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}
resource "aws_iam_role_policy" "hints" {
  name = "hints"
  role = aws_iam_role.hints.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"], Resource = "arn:aws:logs:*:*:*" },
      # Query on the Cost_Store by org partition (never a Scan crossing tenants).
      { Effect = "Allow", Action = ["dynamodb:Query"], Resource = aws_dynamodb_table.cost_store.arn },
      { Effect = "Allow", Action = ["dynamodb:PutItem"], Resource = aws_dynamodb_table.routing_hints.arn },
      # Discovers which orgs exist by reading the gov-config as a contract.
      { Effect = "Allow", Action = ["dynamodb:Scan"], Resource = "arn:aws:dynamodb:${var.region}:${data.aws_caller_identity.current.account_id}:table/${var.project}-${var.environment}-gov-config" },
      # X-Ray requires Resource = "*": trace segments are not addressable
      # resources, so this is the documented AWS pattern for tracing.
      { Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" },
    ]
  })
}
resource "aws_lambda_function" "hints" {
  function_name    = "${local.name}-hints-publisher"
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  role             = aws_iam_role.hints.arn
  filename         = "${local.dist}/hints-publisher.zip"
  source_code_hash = filebase64sha256("${local.dist}/hints-publisher.zip")
  timeout          = 120
  memory_size      = 512
  environment {
    variables = {
      COST_STORE_TABLE = aws_dynamodb_table.cost_store.name
      HINTS_TABLE      = aws_dynamodb_table.routing_hints.name
      CONFIG_TABLE     = "${var.project}-${var.environment}-gov-config"
      WINDOW_DAYS      = "7"
    }
  }

  tracing_config { mode = "Active" }
}

# A 1-HOUR cadence, not 15 minutes: the median of output tokens does not change in
# minutes, and a higher cadence would multiply the Query on the Cost_Store without improving the
# decision. The unavailability signal tolerates this latency because the fallback
# chain keeps acting at call time.
resource "aws_cloudwatch_event_rule" "hints_schedule" {
  name                = "${local.name}-hints-schedule"
  schedule_expression = "rate(1 hour)"
}
resource "aws_cloudwatch_event_target" "hints" {
  rule = aws_cloudwatch_event_rule.hints_schedule.name
  arn  = aws_lambda_function.hints.arn
}
resource "aws_lambda_permission" "hints_schedule" {
  statement_id  = "AllowEventBridgeSchedule"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.hints.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.hints_schedule.arn
}

output "routing_hints_table" { value = aws_dynamodb_table.routing_hints.name }

data "aws_caller_identity" "current" {}

output "usage_queue_url" { value = aws_sqs_queue.usage.url }
output "usage_queue_arn" { value = aws_sqs_queue.usage.arn }
output "cost_store_table" { value = aws_dynamodb_table.cost_store.name }
output "event_bus_name" { value = aws_cloudwatch_event_bus.finops.name }
output "usage_api_endpoint" { value = aws_api_gateway_stage.usage.invoke_url }

# --- Environment Contract (SSM): observability publishes usage_api_url ---
# Table/queue/DLQ/bus names do NOT go to SSM — they are derived from the convention.
resource "aws_ssm_parameter" "usage_api_url" {
  name  = "${local.ssm_root}/observability/usage_api_url"
  type  = "String"
  value = aws_api_gateway_stage.usage.invoke_url
}
