# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Help domain — reusable MODULE (backend/provider live in the env wrapper).
# help-api serves help content (public FAQ + internal deep-dive) to the Console.
# No table: content is go:embed in the binary. IAM for logs only.
terraform {
  required_version = ">= 1.9.0, < 2.0.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = ">= 6.0.0, < 7.0.0" }
  }
}

variable "project" { type = string }
variable "environment" { type = string }
variable "region" { type = string }
variable "dist_path" { type = string }
# Cognito from the Environment Contract (Governance via SSM). Optional override.
# User pool ARN (not issuer/client_id — those were only for the HTTP API's JWT
# authorizer). The REST API's COGNITO_USER_POOLS authorizer validates against the
# whole pool; it does not verify the app client (audience), so the Lambda
# still checks the claim as always.
variable "cognito_user_pool_arn" {
  type    = string
  default = null
}

# Browser origins allowed to call the Help API (CORS), comma-separated.
# Empty (default) = no Access-Control-Allow-Origin header is emitted, so a browser
# on any origin is blocked — deny by default. Server-to-server callers are
# unaffected, since CORS only applies to browsers. Set this to the console origin
# (e.g. https://<console-dist>.cloudfront.net) to read help content from the console.
variable "console_origin" {
  description = "Comma-separated list of browser origins allowed by CORS. Empty = deny all browser origins."
  type        = string
  default     = ""
}

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

locals {
  name     = "${var.project}-${var.environment}-help"
  ssm_root = "/${var.project}/${var.environment}"
  dist     = var.dist_path

  # REST API needs the user pool ARN (COGNITO_USER_POOLS authorizer), not the
  # issuer/audience (those were only for the HTTP API's native JWT authorizer).
  cognito_user_pool_arn = coalesce(var.cognito_user_pool_arn, try(nonsensitive(data.aws_ssm_parameter.cognito_user_pool_arn[0].value), null))
}

data "aws_ssm_parameter" "cognito_user_pool_arn" {
  count = var.cognito_user_pool_arn == null ? 1 : 0
  name  = "${local.ssm_root}/governance/cognito_user_pool_arn"
}

# --- help-api Lambda (Go arm64, provided.al2023) ---
data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "help_api" {
  name               = "${local.name}-api"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

# Least privilege: logs only (content is embedded; no DynamoDB/Secrets).
resource "aws_iam_role_policy" "help_api" {
  name = "help-api"
  role = aws_iam_role.help_api.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"], Resource = "arn:aws:logs:*:*:*" },
      # X-Ray requires Resource = "*": trace segments are not addressable
      # resources, so this is the documented AWS pattern for tracing.
      { Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" },
    ]
  })
}

resource "aws_lambda_function" "help_api" {
  function_name    = "${local.name}-api"
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  role             = aws_iam_role.help_api.arn
  filename         = "${local.dist}/help-api.zip"
  source_code_hash = filebase64sha256("${local.dist}/help-api.zip")
  timeout          = 10
  memory_size      = 128
  environment {
    variables = {
      CONSOLE_ORIGIN = var.console_origin
    }
  }

  tracing_config { mode = "Active" }
}

# --- REST API + native COGNITO_USER_POOLS authorizer ---
# Switched from HTTP API (aws_apigatewayv2_*) to REST API (aws_api_gateway_*):
# HTTP API does not support AWS WAF, resource policies, request validation, or
# private endpoints — REST API supports all of them. REST API has no native JWT
# authorizer, but it has COGNITO_USER_POOLS, which covers the same case (validates the token
# against the user pool directly, without a custom Lambda authorizer).
resource "aws_api_gateway_rest_api" "help" {
  name = "${local.name}-api"
  endpoint_configuration {
    types = ["REGIONAL"]
  }
}

resource "aws_api_gateway_authorizer" "cognito" {
  name                             = "${local.name}-cognito"
  rest_api_id                      = aws_api_gateway_rest_api.help.id
  type                             = "COGNITO_USER_POOLS"
  provider_arns                    = [local.cognito_user_pool_arn]
  identity_source                  = "method.request.header.Authorization"
  authorizer_result_ttl_in_seconds = 300
}

# A single {proxy+} resource dispatches /help/faq and /help/internal to the same
# Lambda — path routing stays in the Go handler (RawPath), same as before.
resource "aws_api_gateway_resource" "proxy" {
  rest_api_id = aws_api_gateway_rest_api.help.id
  parent_id   = aws_api_gateway_rest_api.help.root_resource_id
  path_part   = "{proxy+}"
}

resource "aws_api_gateway_method" "proxy_any" {
  rest_api_id   = aws_api_gateway_rest_api.help.id
  resource_id   = aws_api_gateway_resource.proxy.id
  http_method   = "ANY"
  authorization = "COGNITO_USER_POOLS"
  authorizer_id = aws_api_gateway_authorizer.cognito.id
  request_parameters = {
    "method.request.path.proxy" = true
  }
}

resource "aws_api_gateway_integration" "proxy_any" {
  rest_api_id             = aws_api_gateway_rest_api.help.id
  resource_id             = aws_api_gateway_resource.proxy.id
  http_method             = aws_api_gateway_method.proxy_any.http_method
  integration_http_method = "POST"
  type                    = "AWS_PROXY"
  uri                     = aws_lambda_function.help_api.invoke_arn
}

# OPTIONS without authorizer: a CORS preflight carries no token, so requiring
# Cognito here would keep the browser from even sending the real request.
resource "aws_api_gateway_method" "proxy_options" {
  rest_api_id   = aws_api_gateway_rest_api.help.id
  resource_id   = aws_api_gateway_resource.proxy.id
  http_method   = "OPTIONS"
  authorization = "NONE"
  request_parameters = {
    "method.request.path.proxy" = true
  }
}

resource "aws_api_gateway_integration" "proxy_options" {
  rest_api_id             = aws_api_gateway_rest_api.help.id
  resource_id             = aws_api_gateway_resource.proxy.id
  http_method             = aws_api_gateway_method.proxy_options.http_method
  integration_http_method = "POST"
  type                    = "AWS_PROXY"
  uri                     = aws_lambda_function.help_api.invoke_arn
}

resource "aws_api_gateway_deployment" "help" {
  rest_api_id = aws_api_gateway_rest_api.help.id
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

resource "aws_cloudwatch_log_group" "apigw_help" {
  name              = "/aws/apigateway/${local.name}-help"
  retention_in_days = 90
}

resource "aws_api_gateway_stage" "help" {
  rest_api_id   = aws_api_gateway_rest_api.help.id
  deployment_id = aws_api_gateway_deployment.help.id
  stage_name    = "prod"

  xray_tracing_enabled = true

  # Account-level API Gateway CloudWatch role is owned by the core domain (see manage_apigw_account_settings there).
  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.apigw_help.arn
    format          = local.apigw_access_log_format
  }
}

resource "aws_lambda_permission" "help" {
  statement_id  = "AllowHelpAPI"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.help_api.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_api_gateway_rest_api.help.execution_arn}/*/*"
}

# --- AWS WAF: the one protection HTTP API did not support and that drove this migration ---
resource "aws_wafv2_web_acl" "help" {
  name = "${local.name}-waf"
  # No apostrophes: WAFv2 rejects them in description.
  description = "Standard OWASP protection and rate limit for the help domain REST API."
  scope       = "REGIONAL"

  default_action {
    allow {}
  }

  # AWS managed rules: broad coverage (SQLi, XSS, known-malicious inputs)
  # without maintaining our own rules.
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

  # Rate limit per IP: 2000 req/5min is the floor AWS recommends for an
  # authenticated API — high enough not to bother legitimate use, low
  # enough to contain a client with a bug/loop or a brute-force attempt.
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

resource "aws_wafv2_web_acl_association" "help" {
  resource_arn = aws_api_gateway_stage.help.arn
  web_acl_arn  = aws_wafv2_web_acl.help.arn
}

# --- Environment Contract (SSM): help publishes its endpoint ---
resource "aws_ssm_parameter" "help_api_url" {
  name  = "${local.ssm_root}/help/help_api_url"
  type  = "String"
  value = aws_api_gateway_stage.help.invoke_url
}

output "help_api_endpoint" { value = aws_api_gateway_stage.help.invoke_url }
