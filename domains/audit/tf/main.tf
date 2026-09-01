# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Audit domain — reusable MODULE (backend/provider live in the env wrapper).
#
# Control plane audit trail: who did what in the account.
#
#   emitters (governance/core/backoffice) --PutEvents--> bus --rule--> SQS --> writer --> trail
#   Console / operator Console ----------HTTP (JWT)--------------------> api --> trail
#
# The append-only guarantee is built in three independent layers:
#   1. partition: the org is in the key (pk = AUDIT#<org>), so isolation does not
#      depend on a correct WHERE;
#   2. IAM: the writer only has PutItem (no UpdateItem/DeleteItem) and the api only reads;
#   3. absence of a route: no endpoint deletes or alters a record.
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
variable "cognito_user_pool_arn" {
  type    = string
  default = null
}

# Browser origins allowed to call the Audit API (CORS), comma-separated.
# Empty (default) = no Access-Control-Allow-Origin header is emitted, so a browser
# on any origin is blocked — deny by default. Server-to-server callers are
# unaffected, since CORS only applies to browsers. Set this to the console origin
# (e.g. https://<console-dist>.cloudfront.net) to read the audit trail from the console.
variable "console_origin" {
  description = "Comma-separated list of browser origins allowed by CORS. Empty = deny all browser origins."
  type        = string
  default     = ""
}

data "aws_caller_identity" "current" {}

locals {
  name     = "${var.project}-${var.environment}-aud"
  ssm_root = "/${var.project}/${var.environment}"
  dist     = var.dist_path

  # Governance config table, read as a CONTRACT (the org's plan, for retention
  # and for the tier gate). Named by convention, same as alert-notifier.
  config_table = "${var.project}-${var.environment}-gov-config"

  # REST API needs the user pool ARN (COGNITO_USER_POOLS authorizer), not the
  # issuer/audience (those were only for the HTTP API's native JWT authorizer).
  cognito_user_pool_arn = coalesce(var.cognito_user_pool_arn, try(nonsensitive(data.aws_ssm_parameter.cognito_user_pool_arn[0].value), null))
}

data "aws_ssm_parameter" "cognito_user_pool_arn" {
  count = var.cognito_user_pool_arn == null ? 1 : 0
  name  = "${local.ssm_root}/governance/cognito_user_pool_arn"
}

# =============================== Trail =======================================
# Two LSIs because the dominant access pattern is BY CATEGORY (each Console
# sub-tab is a category) and the second most common is by actor. Resolving a
# category with a FilterExpression would read the entire time window only to
# discard most of it. LSI (not GSI) because the partition is the same — there is
# no reason to repartition. An LSI can only be created together with the table;
# the table is new.
resource "aws_dynamodb_table" "trail" {
  name         = "${local.name}-trail"
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
    name = "cat_sk"
    type = "S"
  }
  attribute {
    name = "actor_sk"
    type = "S"
  }

  local_secondary_index {
    name            = "by_category"
    range_key       = "cat_sk"
    projection_type = "ALL"
  }
  local_secondary_index {
    name            = "by_actor"
    range_key       = "actor_sk"
    projection_type = "ALL"
  }

  # Retention per tier: the writer computes expires_at from the org's plan.
  ttl {
    attribute_name = "expires_at"
    enabled        = true
  }

  # PITR: protection against operational error in a store that, by design, has no
  # correction route.
  point_in_time_recovery { enabled = true }

  # Stream (NEW_IMAGE) feeds the audit-archiver: without it, the 365-day TTL
  # above would mean "disappears from DynamoDB" = "gone forever" — unacceptable
  # for an audit trail. See the "Cold tier (S3)" block below.
  stream_enabled   = true
  stream_view_type = "NEW_IMAGE"
}

# ====================== Cold tier (S3) + archiver ============================
#
# The `trail` table is the HOT tier: fast Query by org/category/actor, it is what
# the Console reads. The 365-day TTL exists so the item does not grow the hot
# storage cost forever — but the TTL is only safe because, before it expires, each
# item has already been copied here. S3 never removes an object on its own (no
# expiration lifecycle configured in this feature — a deliberate decision: the cost
# of keeping everything in S3 Standard/IA is orders of magnitude lower than keeping
# it in DynamoDB, so the pressure to delete the cold tier is much weaker than the
# pressure to delete the hot one).
#
#   trail (DynamoDB) --Streams(NEW_IMAGE)--> archiver (Lambda) --PutObject--> S3
#
# Partitioning by EVENT DATE (year/month/day/org/event_id.json), not ingestion
# date — same rule as the writer's TTL: a record reprocessed from the DLQ days
# later must land on the day it happened, not the day it was reprocessed.
resource "aws_s3_bucket" "archive" {
  bucket_prefix = "${local.name}-archive-"
  force_destroy = false # cold tier: deleting the bucket cannot be an accident of terraform destroy
}

resource "aws_s3_bucket_public_access_block" "archive" {
  bucket                  = aws_s3_bucket.archive.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "archive" {
  bucket = aws_s3_bucket.archive.id
  versioning_configuration {
    status = "Enabled" # the same operational protection as PITR on the table, now on S3
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "archive" {
  bucket = aws_s3_bucket.archive.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_iam_role" "archiver" {
  name               = "${local.name}-archiver"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

# Least privilege, same spirit as the writer: only what is needed to read the
# STREAM (not the table via Query/Scan) and write to S3 (never ListBucket/GetObject —
# this Lambda never reads what it has already archived).
resource "aws_iam_role_policy" "archiver" {
  name = "audit-archiver"
  role = aws_iam_role.archiver.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"], Resource = "arn:aws:logs:*:*:*" },
      { Effect = "Allow", Action = [
        "dynamodb:DescribeStream", "dynamodb:GetRecords", "dynamodb:GetShardIterator", "dynamodb:ListStreams",
      ], Resource = aws_dynamodb_table.trail.stream_arn },
      { Effect = "Allow", Action = ["s3:PutObject"], Resource = "${aws_s3_bucket.archive.arn}/*" },
      # X-Ray requires Resource = "*": trace segments are not addressable
      # resources, so this is the documented AWS pattern for tracing.
      { Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" },
    ]
  })
}

resource "aws_lambda_function" "archiver" {
  function_name    = "${local.name}-archiver"
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  role             = aws_iam_role.archiver.arn
  filename         = "${local.dist}/audit-archiver.zip"
  source_code_hash = filebase64sha256("${local.dist}/audit-archiver.zip")
  timeout          = 30
  memory_size      = 128
  environment {
    variables = {
      ARCHIVE_BUCKET = aws_s3_bucket.archive.id
    }
  }

  tracing_config { mode = "Active" }
}

resource "aws_lambda_event_source_mapping" "archive" {
  event_source_arn  = aws_dynamodb_table.trail.stream_arn
  function_name     = aws_lambda_function.archiver.arn
  starting_position = "LATEST"
  batch_size        = 100
  # Higher retry than the default (the other writers default to the SQS DLQ, but a
  # DynamoDB Stream has no queue of its own) — losing an archiving batch to a
  # transient failure would be worse than reprocessing 3x before giving up on the shard.
  maximum_retry_attempts = 3
}

# ========================= Bus + ingestion queue ============================
resource "aws_cloudwatch_event_bus" "audit" {
  name = "${local.name}-events"
}

# Encryption at rest with SSE-SQS (AWS-managed key). This queue carries the audit
# trail ingest stream, so plaintext at rest would defeat the point of the trail.
resource "aws_sqs_queue" "dlq" {
  name                      = "${local.name}-ingest-dlq"
  message_retention_seconds = 1209600 # 14 days: time to notice and reprocess
  sqs_managed_sse_enabled   = true
}

resource "aws_sqs_queue" "ingest" {
  name                       = "${local.name}-ingest"
  visibility_timeout_seconds = 60
  sqs_managed_sse_enabled    = true
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dlq.arn
    # 5 attempts before the DLQ: a transient DynamoDB failure passes; a persistent
    # failure stays visible instead of disappearing.
    maxReceiveCount = 5
  })
}

resource "aws_sqs_queue_policy" "ingest" {
  queue_url = aws_sqs_queue.ingest.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "events.amazonaws.com" }
      Action    = "sqs:SendMessage"
      Resource  = aws_sqs_queue.ingest.arn
      Condition = { ArnEquals = { "aws:SourceArn" = aws_cloudwatch_event_rule.audit.arn } }
    }]
  })
}

resource "aws_cloudwatch_event_rule" "audit" {
  name           = "${local.name}-ingest"
  event_bus_name = aws_cloudwatch_event_bus.audit.name
  # Matches by detail-type, not by source: the emitters are several domains and the
  # list cannot grow in the audit Terraform with every new emitter.
  event_pattern = jsonencode({ "detail-type" = ["aiplat.audit"] })
}

resource "aws_cloudwatch_event_target" "audit_sqs" {
  rule           = aws_cloudwatch_event_rule.audit.name
  event_bus_name = aws_cloudwatch_event_bus.audit.name
  arn            = aws_sqs_queue.ingest.arn
}

# ============================== Writer =======================================
data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "writer" {
  name               = "${local.name}-writer"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

# Least privilege WITH AN INTEGRITY PURPOSE: only PutItem. No UpdateItem, no
# DeleteItem, no BatchWriteItem (which deletes). That is what makes the trail
# truly append-only and not just by convention.
resource "aws_iam_role_policy" "writer" {
  name = "audit-writer"
  role = aws_iam_role.writer.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"], Resource = "arn:aws:logs:*:*:*" },
      { Effect = "Allow", Action = ["dynamodb:PutItem"], Resource = aws_dynamodb_table.trail.arn },
      # Contract read: the org's plan, to compute retention.
      { Effect = "Allow", Action = ["dynamodb:GetItem"], Resource = "arn:aws:dynamodb:${var.region}:${data.aws_caller_identity.current.account_id}:table/${local.config_table}" },
      { Effect = "Allow", Action = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes"], Resource = aws_sqs_queue.ingest.arn },
      # X-Ray requires Resource = "*": trace segments are not addressable
      # resources, so this is the documented AWS pattern for tracing.
      { Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" },
    ]
  })
}

resource "aws_lambda_function" "writer" {
  function_name    = "${local.name}-writer"
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  role             = aws_iam_role.writer.arn
  filename         = "${local.dist}/audit-writer.zip"
  source_code_hash = filebase64sha256("${local.dist}/audit-writer.zip")
  timeout          = 30
  memory_size      = 128
  environment {
    variables = {
      TRAIL_TABLE  = aws_dynamodb_table.trail.name
      CONFIG_TABLE = local.config_table
    }
  }

  tracing_config { mode = "Active" }
}

resource "aws_lambda_event_source_mapping" "ingest" {
  event_source_arn = aws_sqs_queue.ingest.arn
  function_name    = aws_lambda_function.writer.arn
  batch_size       = 10
}

# ================================ API ========================================
resource "aws_iam_role" "api" {
  name               = "${local.name}-api"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

# The api ONLY READS. No write permission, neither on the trail nor on the config —
# it complements the absence of a write route in the code.
resource "aws_iam_role_policy" "api" {
  name = "audit-api"
  role = aws_iam_role.api.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"], Resource = "arn:aws:logs:*:*:*" },
      { Effect = "Allow", Action = ["dynamodb:Query", "dynamodb:GetItem"], Resource = [
        aws_dynamodb_table.trail.arn,
        "${aws_dynamodb_table.trail.arn}/index/*",
      ] },
      { Effect = "Allow", Action = ["dynamodb:GetItem"], Resource = "arn:aws:dynamodb:${var.region}:${data.aws_caller_identity.current.account_id}:table/${local.config_table}" },
      # X-Ray requires Resource = "*": trace segments are not addressable
      # resources, so this is the documented AWS pattern for tracing.
      { Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" },
    ]
  })
}

resource "aws_lambda_function" "api" {
  function_name    = "${local.name}-api"
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  role             = aws_iam_role.api.arn
  filename         = "${local.dist}/audit-api.zip"
  source_code_hash = filebase64sha256("${local.dist}/audit-api.zip")
  timeout          = 20
  memory_size      = 256
  environment {
    variables = {
      TRAIL_TABLE    = aws_dynamodb_table.trail.name
      CONFIG_TABLE   = local.config_table
      CONSOLE_ORIGIN = var.console_origin
    }
  }

  tracing_config { mode = "Active" }
}

# --- REST API + native COGNITO_USER_POOLS authorizer ---
# Switched from HTTP API to REST API: HTTP API does not support AWS WAF, resource
# policies, request validation, or private endpoints — REST API supports all of them.
# REST API has no native JWT authorizer, but it has COGNITO_USER_POOLS, which covers
# the same case without needing a custom Lambda authorizer.
resource "aws_api_gateway_rest_api" "audit" {
  name = "${local.name}-api"
  endpoint_configuration {
    types = ["REGIONAL"]
  }
}

resource "aws_api_gateway_authorizer" "cognito" {
  name                             = "${local.name}-cognito"
  rest_api_id                      = aws_api_gateway_rest_api.audit.id
  type                             = "COGNITO_USER_POOLS"
  provider_arns                    = [local.cognito_user_pool_arn]
  identity_source                  = "method.request.header.Authorization"
  authorizer_result_ttl_in_seconds = 300
}

# A single {proxy+} resource dispatches /audit/records and /audit/export to the
# same Lambda — path routing stays in the Go handler.
resource "aws_api_gateway_resource" "proxy" {
  rest_api_id = aws_api_gateway_rest_api.audit.id
  parent_id   = aws_api_gateway_rest_api.audit.root_resource_id
  path_part   = "{proxy+}"
}

resource "aws_api_gateway_method" "proxy_any" {
  rest_api_id   = aws_api_gateway_rest_api.audit.id
  resource_id   = aws_api_gateway_resource.proxy.id
  http_method   = "ANY"
  authorization = "COGNITO_USER_POOLS"
  authorizer_id = aws_api_gateway_authorizer.cognito.id
  request_parameters = {
    "method.request.path.proxy" = true
  }
}

resource "aws_api_gateway_integration" "proxy_any" {
  rest_api_id             = aws_api_gateway_rest_api.audit.id
  resource_id             = aws_api_gateway_resource.proxy.id
  http_method             = aws_api_gateway_method.proxy_any.http_method
  integration_http_method = "POST"
  type                    = "AWS_PROXY"
  uri                     = aws_lambda_function.api.invoke_arn
}

# OPTIONS without authorizer: a CORS preflight carries no token, so requiring
# Cognito here would keep the browser from even sending the real request.
resource "aws_api_gateway_method" "proxy_options" {
  rest_api_id   = aws_api_gateway_rest_api.audit.id
  resource_id   = aws_api_gateway_resource.proxy.id
  http_method   = "OPTIONS"
  authorization = "NONE"
  request_parameters = {
    "method.request.path.proxy" = true
  }
}

resource "aws_api_gateway_integration" "proxy_options" {
  rest_api_id             = aws_api_gateway_rest_api.audit.id
  resource_id             = aws_api_gateway_resource.proxy.id
  http_method             = aws_api_gateway_method.proxy_options.http_method
  integration_http_method = "POST"
  type                    = "AWS_PROXY"
  uri                     = aws_lambda_function.api.invoke_arn
}

resource "aws_api_gateway_deployment" "audit" {
  rest_api_id = aws_api_gateway_rest_api.audit.id
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

resource "aws_cloudwatch_log_group" "apigw_audit" {
  name              = "/aws/apigateway/${local.name}-audit"
  retention_in_days = 90
}

resource "aws_api_gateway_stage" "audit" {
  rest_api_id   = aws_api_gateway_rest_api.audit.id
  deployment_id = aws_api_gateway_deployment.audit.id
  stage_name    = "prod"

  xray_tracing_enabled = true

  # Account-level API Gateway CloudWatch role is owned by the core domain (see manage_apigw_account_settings there).
  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.apigw_audit.arn
    format          = local.apigw_access_log_format
  }
}

resource "aws_lambda_permission" "audit" {
  statement_id  = "AllowAuditAPI"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.api.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_api_gateway_rest_api.audit.execution_arn}/*/*"
}

# --- AWS WAF: the one protection HTTP API did not support and that drove this migration ---
resource "aws_wafv2_web_acl" "audit" {
  name = "${local.name}-waf"
  # No apostrophes: WAFv2 rejects them in description.
  description = "Standard OWASP protection and rate limit for the audit domain REST API."
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

resource "aws_wafv2_web_acl_association" "audit" {
  resource_arn = aws_api_gateway_stage.audit.arn
  web_acl_arn  = aws_wafv2_web_acl.audit.arn
}

# ====================== Environment Contract (SSM) ===========================
resource "aws_ssm_parameter" "audit_api_url" {
  name  = "${local.ssm_root}/audit/audit_api_url"
  type  = "String"
  value = aws_api_gateway_stage.audit.invoke_url
}

# The bus name is published so the EMITTERS consume it without hardcoding — it is
# this domain's output contract.
resource "aws_ssm_parameter" "audit_bus_name" {
  name  = "${local.ssm_root}/audit/audit_bus_name"
  type  = "String"
  value = aws_cloudwatch_event_bus.audit.name
}

output "audit_api_endpoint" { value = aws_api_gateway_stage.audit.invoke_url }
output "audit_bus_name" { value = aws_cloudwatch_event_bus.audit.name }
output "audit_bus_arn" { value = aws_cloudwatch_event_bus.audit.arn }
output "trail_table" { value = aws_dynamodb_table.trail.name }
output "ingest_dlq_url" { value = aws_sqs_queue.dlq.id }
output "archive_bucket" { value = aws_s3_bucket.archive.id }
