# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

terraform {
  required_version = ">= 1.9.0, < 2.0.0"
  required_providers {
    aws = {
      source = "hashicorp/aws"
      # 6.64 is the floor because that is where aws_bedrockagentcore_gateway_target gained
      # the `inference` block. It is a HARD requirement even with the gateway disabled: the
      # block has to parse whatever count evaluates to, so an older provider fails with
      # "Blocks of type inference are not expected here" and nothing pointing at a version.
      version = ">= 6.64.0, < 7.0.0"
      # aws.agentcore places the optional AgentCore Gateway in its OWN region, because the
      # model catalog behind the gateway differs by region (see agentcore.tf). Declared as
      # a configuration alias so the CALLER supplies it — a module cannot configure a
      # provider, and inferring the region from a variable inside the module would silently
      # ignore it.
      configuration_aliases = [aws.agentcore]
    }
  }
}
