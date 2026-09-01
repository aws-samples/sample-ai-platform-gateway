#!/bin/bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
set -e
# Resolve paths relative to this script so it works from any checkout location.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/../domains/governance/envs/poc"
export TF_PLUGIN_CACHE_DIR="${TF_PLUGIN_CACHE_DIR:-$HOME/.terraform.d/plugin-cache}"
export AWS_REGION="${AWS_REGION:-us-west-2}"

echo "=== Terraform init ==="
terraform init -input=false

echo "=== Terraform apply ==="
terraform apply -auto-approve

echo "=== DEPLOY DONE ==="
