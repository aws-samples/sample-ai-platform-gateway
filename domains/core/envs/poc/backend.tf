# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Remote state on S3. A local state file is not acceptable for anything but a
# throwaway experiment: it is not shared, not versioned, not encrypted at rest,
# and it is trivially lost with the workstation — while containing every resource
# id (and any value Terraform marks sensitive) of the deployment.
#
# The bucket/key/region are DELIBERATELY not hardcoded (partial configuration):
# they are deployment-specific and a bucket name must never be committed. Supply
# them at init time:
#
#   terraform init \
#     -backend-config="bucket=<your-tf-state-bucket>" \
#     -backend-config="key=aiplat/poc/core/terraform.tfstate" \
#     -backend-config="region=<your-region>"
#
# On Terraform >= 1.10 add -backend-config="use_lockfile=true" for S3-native
# state locking (no DynamoDB table required).
#
# MIGRATING an existing local state: append -migrate-state to the command above;
# Terraform copies terraform.tfstate into the bucket. Do it once, per domain.
#
# The bucket itself should have versioning ENABLED (state history is the only
# undo you get) and default encryption.
terraform {
  backend "s3" {
    encrypt = true
  }
}
