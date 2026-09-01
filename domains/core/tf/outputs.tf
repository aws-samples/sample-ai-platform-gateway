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
