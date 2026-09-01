# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Remote state on S3 — see domains/core/envs/poc/backend.tf for the rationale.
# Partial configuration: supply bucket/key/region at init time.
#
#   terraform init \
#     -backend-config="bucket=<your-tf-state-bucket>" \
#     -backend-config="key=aiplat/poc/help/terraform.tfstate" \
#     -backend-config="region=<your-region>"
#
# Add -migrate-state the first time, to move an existing local state into S3.
terraform {
  backend "s3" {
    encrypt = true
  }
}
