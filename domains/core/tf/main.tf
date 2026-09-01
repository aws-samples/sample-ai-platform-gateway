# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Core domain (the gateway) — isolated stack. Lambda in Go (arm64/Graviton, provided.al2023).
# Entry via Lambda Function URL with response streaming (SSE). Auth is done in the handler.
# NOTE: the AWS resources keep the "-inf" prefix (renaming would recreate tables/URL).
data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

locals {
  name       = "${var.project}-${var.environment}-inf"
  dist       = var.dist_path
  account_id = data.aws_caller_identity.current.account_id
  region     = data.aws_region.current.name
  ssm_root   = "/${var.project}/${var.environment}"

  # Identifiers of OTHER domains — derived from the convention (not literals),
  # with an optional override. Queue URL/ARN built with the current account/region.
  config_table_name = coalesce(var.config_table_name, "${var.project}-${var.environment}-gov-config")
  # Routing_Hints belongs to the Observability domain; we derive it from the
  # convention, like the other external identifiers — without coupling state between domains.
  hints_table_name = coalesce(var.hints_table_name, "${var.project}-${var.environment}-obs-routing-hints")
  usage_queue_name = "${var.project}-${var.environment}-obs-usage"
  usage_queue_arn  = coalesce(var.usage_queue_arn, "arn:aws:sqs:${local.region}:${local.account_id}:${local.usage_queue_name}")
  usage_queue_url  = coalesce(var.usage_queue_url, "https://sqs.${local.region}.amazonaws.com/${local.account_id}/${local.usage_queue_name}")

  # Audit bus (Audit domain), by convention like the other external identifiers.
  # Convention instead of a data source avoids an ORDER dependency between the
  # stacks: the Core must be able to come up even without audit existing.
  audit_bus     = coalesce(var.audit_bus, "${var.project}-${var.environment}-aud-events")
  audit_bus_arn = "arn:aws:events:${local.region}:${local.account_id}:event-bus/${local.audit_bus}"

  # REST API needs the user pool ARN (COGNITO_USER_POOLS authorizer), not the
  # issuer/audience (those were only for the HTTP API's native JWT authorizer).
  cognito_user_pool_arn = coalesce(var.cognito_user_pool_arn, try(nonsensitive(data.aws_ssm_parameter.cognito_user_pool_arn[0].value), null))
  # The single org of this deployment (single-org), from the Environment Contract
  # (governance via SSM). The Core needs it to build the SAME scope key
  # (ORG#<org>#TEAM#...#APP#...) that Governance already writes in gov-config —
  # without it, the team/app config set in the console never reached the gateway.
  deployment_org = coalesce(var.deployment_org, try(nonsensitive(data.aws_ssm_parameter.deployment_org[0].value), "XYZ_ORG"))
}

data "aws_ssm_parameter" "cognito_user_pool_arn" {
  count = var.cognito_user_pool_arn == null ? 1 : 0
  name  = "${local.ssm_root}/governance/cognito_user_pool_arn"
}
data "aws_ssm_parameter" "deployment_org" {
  count = var.deployment_org == null ? 1 : 0
  name  = "${local.ssm_root}/governance/deployment_org"
}

# --- Domain-owned data ---
resource "aws_dynamodb_table" "cache" {
  name         = "${local.name}-cache"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "cache_key"
  attribute {
    name = "cache_key"
    type = "S"
  }
  ttl {
    attribute_name = "expires_at"
    enabled        = true
  }
  # PITR is enabled on every table for uniformity. This one is regenerable
  # (derived cache with TTL), so it is the cheapest place to relax it if the
  # operator wants to trim cost.
  point_in_time_recovery { enabled = true }
}

resource "aws_dynamodb_table" "api_keys" {
  name         = "${local.name}-api-keys"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "api_key_hash"
  attribute {
    name = "api_key_hash"
    type = "S"
  }
  # PITR: this table holds the tenant credential hashes. An accidental delete
  # here locks every caller out, and there is no other copy of it.
  point_in_time_recovery { enabled = true }
}

# Operational enforcement counters (rate limit per window + monthly spend).
# Core-owned data: avoids a synchronous read of Observability's Cost_Store.
# The Cost_Store remains the historical/authoritative source.
resource "aws_dynamodb_table" "limits" {
  name         = "${local.name}-limits"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  attribute {
    name = "pk"
    type = "S"
  }
  ttl {
    attribute_name = "expires_at"
    enabled        = true
  }
  # PITR: losing the counters loses the month's spend enforcement.
  point_in_time_recovery { enabled = true }
}

# --- Execution role (least privilege) ---
data "aws_iam_policy_document" "assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "router" {
  name               = "${local.name}-router"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}
resource "aws_iam_role_policy" "router" {
  name = "router"
  role = aws_iam_role.router.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"], Resource = "arn:aws:logs:*:*:*" },
      { Effect = "Allow", Action = ["dynamodb:GetItem", "dynamodb:PutItem"], Resource = aws_dynamodb_table.cache.arn },
      { Effect = "Allow", Action = ["dynamodb:GetItem"], Resource = aws_dynamodb_table.api_keys.arn },
      { Effect = "Allow", Action = ["dynamodb:GetItem", "dynamodb:UpdateItem"], Resource = aws_dynamodb_table.limits.arn },
      # BatchGetItem to resolve the effective config (global → ORG → TEAM → APP).
      { Effect = "Allow", Action = ["dynamodb:GetItem", "dynamodb:BatchGetItem"], Resource = "arn:aws:dynamodb:*:*:table/${local.config_table_name}" },
      # Observability's Routing_Hints, read as a CONTRACT. Least privilege:
      # only GetItem, only on that table — the Core does not write another domain's data.
      { Effect = "Allow", Action = ["dynamodb:GetItem"], Resource = "arn:aws:dynamodb:*:*:table/${local.hints_table_name}" },
      # Platform secrets (pooled) and BYO per org (aiplat/org/<org>/*).
      { Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = [var.secret_prefix_arn, "arn:aws:secretsmanager:*:*:secret:aiplat/org/*"] },
      # BYO Bedrock: assume the role the CLIENT creates in their own account.
      # Restricted by naming convention so it cannot assume any arbitrary role.
      { Effect = "Allow", Action = ["sts:AssumeRole"], Resource = "arn:aws:iam::*:role/AIPlatGatewayAccess*" },
      { Effect = "Allow", Action = ["sqs:SendMessage"], Resource = local.usage_queue_arn },
      # Pooled Bedrock invocation, scoped to model resources instead of "*": the
      # gateway only ever calls foundation models (and cross-region inference
      # profiles), never any other Bedrock resource (no agent, no knowledge base,
      # no fine-tuning job). The model id itself is not knowable in advance — it is
      # whatever the operator configures — so the wildcard stays on the model id,
      # not on the resource TYPE.
      {
        Effect = "Allow",
        Action = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"],
        Resource = [
          "arn:aws:bedrock:*::foundation-model/*",
          "arn:aws:bedrock:*:*:inference-profile/*",
        ]
      },
      # X-Ray requires Resource = "*": trace segments are not addressable
      # resources, so this is the documented AWS pattern for tracing.
      { Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" },
    ]
  })
}

# --- Go Lambda (arm64) ---
resource "aws_lambda_function" "router" {
  function_name    = "${local.name}-router"
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  role             = aws_iam_role.router.arn
  filename         = "${local.dist}/router.zip"
  source_code_hash = filebase64sha256("${local.dist}/router.zip")
  timeout          = 120
  memory_size      = 256
  environment {
    variables = {
      CONFIG_TABLE   = local.config_table_name
      CACHE_TABLE    = aws_dynamodb_table.cache.name
      API_KEYS_TABLE = aws_dynamodb_table.api_keys.name
      LIMITS_TABLE   = aws_dynamodb_table.limits.name
      HINTS_TABLE    = local.hints_table_name
      # Sample threshold to trust the hint. The code default is 20 (production);
      # in a low-traffic environment a high value would make the hint never be used.
      MIN_HINT_SAMPLES = tostring(var.min_hint_samples)
      USAGE_QUEUE_URL  = local.usage_queue_url
      MODEL_ROUTING    = var.model_routing_fallback
      PRICING_TABLE    = var.pricing_fallback
      # This deployment's org (Environment Contract, via SSM) — used to build the
      # SAME scope key that Governance writes in gov-config.
      DEPLOYMENT_ORG = local.deployment_org
      # Browser origins allowed to call this API (comma-separated). Empty = no
      # CORS headers are emitted at all, which is the safe default: a browser on
      # another origin gets blocked. Server-to-server SDK callers are unaffected
      # (they send no Origin header). Set it to the console origin to use the
      # Playground.
      CONSOLE_ORIGIN = var.console_origin
    }
  }

  tracing_config { mode = "Active" }
}

# --- Entry: API Gateway REST API (in-handler auth) ---
# Why not a Lambda Function URL: (1) a public URL is blocked by the account guardrail
# (Palisade/Epoxy: "world accessible"); (2) with OAC/IAM, a POST with a body requires the CLIENT
# to send x-amz-content-sha256 (AWS docs: "Lambda doesn't support unsigned payloads"),
# which breaks the drop-in OpenAI SDK. Trade-off: API GW buffers → SSE is not incremental
# (it was so on HTTP API and stays so on REST API — the migration does not change that trade-off).
# Switched from HTTP API to REST API for the same reason as the other domains: AWS WAF,
# resource policies and private endpoints do not exist in HTTP API. No authorizer:
# this endpoint authenticates by its own API key, verified INSIDE the Go handler
# (authResolve in internal/gateway) — never by Cognito.
resource "aws_api_gateway_rest_api" "this" {
  name = "${local.name}-api"
  endpoint_configuration {
    types = ["REGIONAL"]
  }
}

resource "aws_api_gateway_resource" "router_proxy" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  parent_id   = aws_api_gateway_rest_api.this.root_resource_id
  path_part   = "{proxy+}"
}

resource "aws_api_gateway_method" "router_proxy_any" {
  rest_api_id   = aws_api_gateway_rest_api.this.id
  resource_id   = aws_api_gateway_resource.router_proxy.id
  http_method   = "ANY"
  authorization = "NONE"
  request_parameters = {
    "method.request.path.proxy" = true
  }
}

resource "aws_api_gateway_integration" "router_proxy_any" {
  rest_api_id             = aws_api_gateway_rest_api.this.id
  resource_id             = aws_api_gateway_resource.router_proxy.id
  http_method             = aws_api_gateway_method.router_proxy_any.http_method
  integration_http_method = "POST"
  type                    = "AWS_PROXY"
  uri                     = aws_lambda_function.router.invoke_arn
  timeout_milliseconds    = 29000
}

resource "aws_api_gateway_deployment" "router" {
  rest_api_id = aws_api_gateway_rest_api.this.id
  triggers = {
    redeployment = sha1(jsonencode([
      aws_api_gateway_resource.router_proxy.id,
      aws_api_gateway_method.router_proxy_any.id,
      aws_api_gateway_integration.router_proxy_any.id,
    ]))
  }
  lifecycle {
    create_before_destroy = true
  }
}

# --- Access logging (API Gateway) ---------------------------------------------
# API Gateway needs an ACCOUNT-level CloudWatch role to write logs, and that
# setting is a singleton per account+region. It is created here (the core domain)
# so exactly one domain owns it; set manage_apigw_account_settings = false if the
# account is already configured, otherwise two stacks would fight over it.
resource "aws_iam_role" "apigw_cloudwatch" {
  count = var.manage_apigw_account_settings ? 1 : 0
  name  = "${local.name}-apigw-cloudwatch"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "apigateway.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}
resource "aws_iam_role_policy_attachment" "apigw_cloudwatch" {
  count      = var.manage_apigw_account_settings ? 1 : 0
  role       = aws_iam_role.apigw_cloudwatch[0].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonAPIGatewayPushToCloudWatchLogs"
}
resource "aws_api_gateway_account" "this" {
  count               = var.manage_apigw_account_settings ? 1 : 0
  cloudwatch_role_arn = aws_iam_role.apigw_cloudwatch[0].arn
  depends_on          = [aws_iam_role_policy_attachment.apigw_cloudwatch]
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

resource "aws_cloudwatch_log_group" "apigw_router" {
  name              = "/aws/apigateway/${local.name}-router"
  retention_in_days = 90
}

resource "aws_api_gateway_stage" "router" {
  rest_api_id   = aws_api_gateway_rest_api.this.id
  deployment_id = aws_api_gateway_deployment.router.id
  stage_name    = "prod"

  xray_tracing_enabled = true

  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.apigw_router.arn
    format          = local.apigw_access_log_format
  }

  depends_on = [aws_api_gateway_account.this]
}

resource "aws_lambda_permission" "api" {
  statement_id  = "AllowAPIGateway"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.router.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_api_gateway_rest_api.this.execution_arn}/*/*"
}

# --- AWS WAF: the one protection HTTP API did not support and that drove this migration ---
#
# BUG FIXED (2026-08-24): 403 {"message":"Forbidden"} on /v1/chat/completions
# when the body exceeded ~8KB — in practice, any call with tool-calling and
# ~16 or more real tools (full schema + description). Root cause: the managed rule
# SizeRestrictions_BODY, inside AWSManagedRulesCommonRuleSet (CRS), has a FIXED
# 8KB blocking threshold — this is INDEPENDENT of the
# request_body.size_inspection_limit in the association_config below, which only controls
# how much of the body is SENT to the WAF for inspection (up to 64KB, at additional cost),
# not the size-based blocking threshold of the CRS rule itself.
# Fixed following AWS's official practice for this case (repost.aws/
# knowledge-center/waf-http-request-body-inspection): apply a Count override
# specifically on the SizeRestrictions_BODY rule (keeping the rest of the CRS —
# SQLi, XSS, etc. — blocking normally) and raise size_inspection_limit to the
# maximum (64KB), so the rules that MATTER (malicious content, not size)
# keep inspecting the whole body instead of only the first 8-16KB.
resource "aws_wafv2_web_acl" "router" {
  name = "${local.name}-router-waf"
  # No apostrophes: WAFv2 validates description against
  # ^[\w+=:#@/\-,\.][\w+=:#@/\-,\.\s]+[\w+=:#@/\-,\.]$ and rejects them.
  description = "Standard OWASP protection and rate limit for the core domain gateway REST API."
  scope       = "REGIONAL"

  default_action {
    allow {}
  }

  # The whole body (up to 64KB) reaches the WAF for content inspection — without this,
  # large tool-calling payloads would be only partially inspected by the content
  # rules (SQLi/XSS), which is worse for security than the default oversize
  # handling (CONTINUE) would resolve on its own.
  association_config {
    request_body {
      api_gateway {
        default_size_inspection_limit = "KB_64"
      }
    }
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

        # Only the body SIZE rule is downgraded to Count — it is the one that
        # produces the false positive on legitimate tool-calling payloads. The other
        # ~15 CRS rules (SQLi, XSS, LFI, etc.) keep blocking normally.
        rule_action_override {
          name = "SizeRestrictions_BODY"
          action_to_use {
            count {}
          }
        }
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name}-router-common"
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
      metric_name                = "${local.name}-router-badinputs"
      sampled_requests_enabled   = true
    }
  }

  # A much higher limit than the other domains: this is the inference gateway
  # itself — every real client-app call goes through here, not just operators
  # in the console. 2000/5min would choke legitimate production use.
  rule {
    name     = "RateLimitPerIP"
    priority = 3
    action {
      block {}
    }
    statement {
      rate_based_statement {
        limit              = 20000
        aggregate_key_type = "IP"
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name}-router-ratelimit"
      sampled_requests_enabled   = true
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = "${local.name}-router-waf"
    sampled_requests_enabled   = true
  }
}

resource "aws_wafv2_web_acl_association" "router" {
  resource_arn = aws_api_gateway_stage.router.arn
  web_acl_arn  = aws_wafv2_web_acl.router.arn
}

# --- keyadmin: API key issuance/management (admin API) ---
# The gate for this API is the API Gateway COGNITO_USER_POOLS authorizer, not a
# shared token. There is deliberately no admin_token variable here: keyadmin's
# Go code never compared ADMIN_TOKEN, so a phantom token variable would mislead
# a reader into relaxing the authorizer believing a second gate exists.

resource "aws_iam_role" "keyadmin" {
  name               = "${local.name}-keyadmin"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}
resource "aws_iam_role_policy" "keyadmin" {
  name = "keyadmin"
  role = aws_iam_role.keyadmin.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"], Resource = "arn:aws:logs:*:*:*" },
      # GetItem is needed for the OWNERSHIP check before revoking.
      { Effect = "Allow", Action = ["dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:DeleteItem", "dynamodb:Scan"], Resource = aws_dynamodb_table.api_keys.arn },
      # CONTRACT read of the teams/apps registry (gov-config) to validate
      # existence at issuance time. Least privilege: only GetItem, only on that table.
      { Effect = "Allow", Action = ["dynamodb:GetItem"], Resource = "arn:aws:dynamodb:*:*:table/${local.config_table_name}" },
      # Audit: publishes an event ABOUT ITSELF on the Audit domain bus. Only
      # PutEvents, only on that bus — the Core does not write to the other domain's store.
      { Effect = "Allow", Action = ["events:PutEvents"], Resource = local.audit_bus_arn },
      # X-Ray requires Resource = "*": trace segments are not addressable
      # resources, so this is the documented AWS pattern for tracing.
      { Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" },
    ]
  })
}

resource "aws_lambda_function" "keyadmin" {
  function_name    = "${local.name}-keyadmin"
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  role             = aws_iam_role.keyadmin.arn
  filename         = "${local.dist}/keyadmin.zip"
  source_code_hash = filebase64sha256("${local.dist}/keyadmin.zip")
  timeout          = 15
  memory_size      = 128
  environment {
    variables = {
      API_KEYS_TABLE = aws_dynamodb_table.api_keys.name
      # No ADMIN_TOKEN here on purpose: the gate for this API is the API Gateway
      # COGNITO_USER_POOLS authorizer. keyadmin never read the token, so
      # injecting one would suggest a second gate that does not exist.
      CONFIG_TABLE = local.config_table_name
      # Empty TURNS OFF emission without breaking anything.
      AUDIT_BUS = local.audit_bus
      # Browser origins allowed by CORS (comma-separated). Empty = deny all
      # browser origins (no header emitted). This is an ADMIN API, so it should
      # only ever list the console origin.
      CONSOLE_ORIGIN = var.console_origin
    }
  }

  tracing_config { mode = "Active" }
}

# --- REST API + native COGNITO_USER_POOLS authorizer ---
# Switched from HTTP API to REST API: HTTP API does not support AWS WAF, resource
# policies, request validation, or private endpoints — REST API supports all of them.
# REST API has no native JWT authorizer, but it has COGNITO_USER_POOLS, which covers
# the same case without needing a custom Lambda authorizer. (The main router/gateway
# stays out of this migration: it authenticates by its own API key in the
# handler, not by Cognito, and remains on HTTP API.)
resource "aws_api_gateway_rest_api" "keyadmin" {
  name = "${local.name}-keyadmin"
  endpoint_configuration {
    types = ["REGIONAL"]
  }
}

resource "aws_api_gateway_authorizer" "cognito" {
  name                             = "${local.name}-cognito"
  rest_api_id                      = aws_api_gateway_rest_api.keyadmin.id
  type                             = "COGNITO_USER_POOLS"
  provider_arns                    = [local.cognito_user_pool_arn]
  identity_source                  = "method.request.header.Authorization"
  authorizer_result_ttl_in_seconds = 300
}

resource "aws_api_gateway_resource" "keyadmin_proxy" {
  rest_api_id = aws_api_gateway_rest_api.keyadmin.id
  parent_id   = aws_api_gateway_rest_api.keyadmin.root_resource_id
  path_part   = "{proxy+}"
}

resource "aws_api_gateway_method" "keyadmin_proxy_any" {
  rest_api_id   = aws_api_gateway_rest_api.keyadmin.id
  resource_id   = aws_api_gateway_resource.keyadmin_proxy.id
  http_method   = "ANY"
  authorization = "COGNITO_USER_POOLS"
  authorizer_id = aws_api_gateway_authorizer.cognito.id
  request_parameters = {
    "method.request.path.proxy" = true
  }
}

resource "aws_api_gateway_integration" "keyadmin_proxy_any" {
  rest_api_id             = aws_api_gateway_rest_api.keyadmin.id
  resource_id             = aws_api_gateway_resource.keyadmin_proxy.id
  http_method             = aws_api_gateway_method.keyadmin_proxy_any.http_method
  integration_http_method = "POST"
  type                    = "AWS_PROXY"
  uri                     = aws_lambda_function.keyadmin.invoke_arn
}

# OPTIONS without authorizer: a CORS preflight carries no token, so requiring
# Cognito here would keep the browser from even sending the real request.
resource "aws_api_gateway_method" "keyadmin_proxy_options" {
  rest_api_id   = aws_api_gateway_rest_api.keyadmin.id
  resource_id   = aws_api_gateway_resource.keyadmin_proxy.id
  http_method   = "OPTIONS"
  authorization = "NONE"
  request_parameters = {
    "method.request.path.proxy" = true
  }
}

resource "aws_api_gateway_integration" "keyadmin_proxy_options" {
  rest_api_id             = aws_api_gateway_rest_api.keyadmin.id
  resource_id             = aws_api_gateway_resource.keyadmin_proxy.id
  http_method             = aws_api_gateway_method.keyadmin_proxy_options.http_method
  integration_http_method = "POST"
  type                    = "AWS_PROXY"
  uri                     = aws_lambda_function.keyadmin.invoke_arn
}

resource "aws_api_gateway_deployment" "keyadmin" {
  rest_api_id = aws_api_gateway_rest_api.keyadmin.id
  triggers = {
    redeployment = sha1(jsonencode([
      aws_api_gateway_resource.keyadmin_proxy.id,
      aws_api_gateway_method.keyadmin_proxy_any.id,
      aws_api_gateway_integration.keyadmin_proxy_any.id,
      aws_api_gateway_method.keyadmin_proxy_options.id,
      aws_api_gateway_integration.keyadmin_proxy_options.id,
      aws_api_gateway_authorizer.cognito.id,
    ]))
  }
  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_cloudwatch_log_group" "apigw_keyadmin" {
  name              = "/aws/apigateway/${local.name}-keyadmin"
  retention_in_days = 90
}

resource "aws_api_gateway_stage" "keyadmin" {
  rest_api_id   = aws_api_gateway_rest_api.keyadmin.id
  deployment_id = aws_api_gateway_deployment.keyadmin.id
  stage_name    = "prod"

  xray_tracing_enabled = true

  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.apigw_keyadmin.arn
    format          = local.apigw_access_log_format
  }

  depends_on = [aws_api_gateway_account.this]
}

resource "aws_lambda_permission" "keyadmin" {
  statement_id  = "AllowKeyAdmin"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.keyadmin.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_api_gateway_rest_api.keyadmin.execution_arn}/*/*"
}

# --- AWS WAF: the one protection HTTP API did not support and that drove this migration ---
resource "aws_wafv2_web_acl" "keyadmin" {
  name        = "${local.name}-keyadmin-waf"
  description = "Standard OWASP protection and rate limit for the keyadmin REST API, core domain."
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
      metric_name                = "${local.name}-keyadmin-common"
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
      metric_name                = "${local.name}-keyadmin-badinputs"
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
      metric_name                = "${local.name}-keyadmin-ratelimit"
      sampled_requests_enabled   = true
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = "${local.name}-keyadmin-waf"
    sampled_requests_enabled   = true
  }
}

resource "aws_wafv2_web_acl_association" "keyadmin" {
  resource_arn = aws_api_gateway_stage.keyadmin.arn
  web_acl_arn  = aws_wafv2_web_acl.keyadmin.arn
}

output "keyadmin_endpoint" { value = aws_api_gateway_stage.keyadmin.invoke_url }

# --- Environment Contract (SSM): core publishes its endpoints (non-derivable) ---
resource "aws_ssm_parameter" "gateway_url" {
  name  = "${local.ssm_root}/core/gateway_url"
  type  = "String"
  value = aws_api_gateway_stage.router.invoke_url
}
resource "aws_ssm_parameter" "keyadmin_url" {
  name  = "${local.ssm_root}/core/keyadmin_url"
  type  = "String"
  value = aws_api_gateway_stage.keyadmin.invoke_url
}

# --- Platform monitoring in CloudWatch (cheap, aggregated) ---
# Each domain instruments itself (see steering aiplat-domains.md). The Core
# emits the "AIPLAT_SLI_FAIL" token only on a failure that counts against US (provider
# unavailable + platform/AWS). A metric filter (free) counts those lines
# into an aggregated metric (no per-org dimension → no cardinality explosion),
# and an alarm notifies the operator on a SYSTEMIC incident — not on an isolated error.

variable "platform_error_alarm_threshold" {
  type        = number
  default     = 10
  description = "Number of sli-eligible failures in 5 min that triggers the operator's systemic alarm."
}

# Topic the alarm publishes to. The subscription (email/Slack/PagerDuty) is
# wired by the operator later — we do not hardcode a destination here (avoids email by mistake).
resource "aws_sns_topic" "ops_alerts" {
  name = "${local.name}-ops-alerts"
  # Encryption at rest with the AWS-managed key for SNS. Alarm payloads name the
  # deployment and the failing metric, so they should not sit in plaintext.
  kms_master_key_id = "alias/aws/sns"
}

# Metric filter on the log group (auto-created by the Lambda) — referenced by name.
# The bare token matches even with the runtime prefix. Free; only the custom metric (~$0.30/month).
resource "aws_cloudwatch_log_metric_filter" "platform_errors" {
  name           = "${local.name}-platform-errors"
  log_group_name = "/aws/lambda/${aws_lambda_function.router.function_name}"
  pattern        = "AIPLAT_SLI_FAIL"
  metric_transformation {
    name          = "PlatformEligibleErrors"
    namespace     = "AIPlat/Gateway"
    value         = "1"
    default_value = "0"
    unit          = "Count"
  }
}

# Aggregated alarm: sum of our failures over 5 min. It is the operator's systemic
# incident signal (fast burn), not a per-request alert.
resource "aws_cloudwatch_metric_alarm" "platform_errors" {
  alarm_name          = "${local.name}-platform-errors"
  alarm_description   = "Gateway platform/dependency failures (sli-eligible) above the threshold in 5 min. Sign of a systemic incident."
  namespace           = "AIPlat/Gateway"
  metric_name         = "PlatformEligibleErrors"
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = var.platform_error_alarm_threshold
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.ops_alerts.arn]
  ok_actions          = [aws_sns_topic.ops_alerts.arn]
}

output "ops_alerts_topic_arn" { value = aws_sns_topic.ops_alerts.arn }
