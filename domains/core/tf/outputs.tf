# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

output "gateway_url" {
  description = "Public endpoint of Core (the gateway) — API Gateway REST API."
  value       = aws_api_gateway_stage.router.invoke_url
}
output "api_keys_table" {
  value = aws_dynamodb_table.api_keys.name
}
output "cache_table" {
  value = aws_dynamodb_table.cache.name
}
output "router_fn" {
  value = aws_lambda_function.router.function_name
}

# AgentCore Gateway endpoint, when the optional gateway is enabled. This is the value a
# `bedrock_gateway` route puts in base_url; null when the gateway was not created.
output "agentcore_gateway_url" {
  description = "Amazon Bedrock AgentCore Gateway endpoint (base_url for provider bedrock_gateway)."
  value       = var.agentcore_gateway_enabled ? aws_bedrockagentcore_gateway.inference[0].gateway_url : null
}
output "agentcore_gateway_id" {
  value = var.agentcore_gateway_enabled ? aws_bedrockagentcore_gateway.inference[0].gateway_id : null
}
output "agentcore_gateway_region" {
  description = "Region the gateway lives in — the signing region for bedrock_gateway routes."
  value       = var.agentcore_gateway_enabled ? var.agentcore_gateway_region : null
}
