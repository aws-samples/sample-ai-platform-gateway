# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

provider "aws" {
  region = var.region
  default_tags {
    tags = {
      Project       = var.project
      Environment   = var.environment
      ManagedBy     = "terraform"
      Domain        = "core"
      "auto-delete" = "no"
    }
  }
}
