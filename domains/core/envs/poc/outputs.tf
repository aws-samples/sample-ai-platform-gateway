# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

output "gateway_url" { value = module.domain.gateway_url }
output "api_keys_table" { value = module.domain.api_keys_table }
output "cache_table" { value = module.domain.cache_table }
output "router_fn" { value = module.domain.router_fn }
output "keyadmin_endpoint" { value = module.domain.keyadmin_endpoint }
