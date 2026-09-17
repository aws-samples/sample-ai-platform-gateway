# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

locals {
  default_tags = {
    Project       = var.project
    Environment   = var.environment
    ManagedBy     = "terraform"
    Domain        = "core"
    "auto-delete" = "no"
  }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = local.default_tags
  }
}

# Second provider for the OPTIONAL AgentCore Gateway, which lives in its own region because
# the model catalog behind it differs by region (see domains/core/tf/agentcore.tf). It is
# declared unconditionally — Terraform has no conditional provider blocks — and simply goes
# unused when agentcore_gateway_enabled is false.
provider "aws" {
  alias  = "agentcore"
  region = var.agentcore_gateway_region
  default_tags {
    tags = local.default_tags
  }
}
