# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Amazon Bedrock AgentCore Gateway — OPTIONAL transport for the `bedrock_gateway` provider.
#
# Everything here is gated on var.agentcore_gateway_enabled and defaults to OFF. The
# gateway is an alternative door to the same models the `bedrock` provider already reaches
# directly, so it is a comparison and a choice, not a dependency: a deployment that never
# sets the flag is byte-identical to one without this file.
#
# WHY A SEPARATE REGION
#
# The gateway can only serve models its endpoint serves, and the catalogs differ by region.
# Measured on 2026-09-16 against this account: bedrock-mantle in us-west-2 lists 49 models
# with exactly ONE Anthropic entry (claude-haiku-4-5), while us-east-1 lists 55 with six
# (haiku-4-5, sonnet-5, opus-4-7, opus-4-8, opus-5, fable-5). A routing ladder needs more
# than one rung, so the gateway is placed by its own region variable rather than inheriting
# the deployment's. That is what the aws.agentcore provider alias is for.
#
# WHY TWO TARGETS
#
# They cover different halves of the catalog and the gateway routes between them on the
# model id:
#
#   mantle  (connector)  → anthropic.claude-{haiku-4-5,sonnet-5,opus-4-7,opus-4-8,opus-5}
#   runtime (provider)   → us./global.anthropic.claude-{opus-4-7,opus-4-8,opus-5,sonnet-5}
#
# Only the mantle catalog carries Haiku 4.5, and only bedrock-runtime carries the
# geo-prefixed inference profiles. One target alone leaves a gap in the ladder.
#
# WHAT THIS CANNOT REACH, and why it is not a bug here
#
# The gateway accepts three inference paths (/v1/messages, /v1/chat/completions,
# /v1/responses) and REJECTS a model id containing ':'. Bedrock's Converse/InvokeModel APIs
# are not among those paths and every versioned Bedrock id ends in "-v1:0", so Amazon Nova,
# Meta Llama, Cohere embeddings and the older Claude generations are unreachable through
# this provider. Routes for those keep using provider "bedrock".

resource "aws_iam_role" "agentcore_gateway" {
  count = var.agentcore_gateway_enabled ? 1 : 0
  name  = "${local.name}-agentcore-gateway"

  # The gateway assumes this role to call the model. SourceAccount and SourceArn are the
  # confused-deputy mitigation AWS's own gateway documentation asks for: without them any
  # gateway in any account that the service can reach could be used to assume this role.
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "bedrock-agentcore.amazonaws.com" }
      Action    = "sts:AssumeRole"
      Condition = {
        StringEquals = { "aws:SourceAccount" = local.account_id }
        ArnLike      = { "aws:SourceArn" = "arn:aws:bedrock-agentcore:${var.agentcore_gateway_region}:${local.account_id}:gateway/*" }
      }
    }]
  })
}

resource "aws_iam_role_policy" "agentcore_gateway" {
  count = var.agentcore_gateway_enabled ? 1 : 0
  name  = "invoke-models"
  role  = aws_iam_role.agentcore_gateway[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      # Scoped to model resources, the same way the router's own Bedrock statement is: the
      # gateway calls foundation models and inference profiles, never another Bedrock
      # resource. The model id stays a wildcard because it is whatever the operator
      # configures, but the resource TYPE does not have to be.
      {
        Effect = "Allow"
        Action = [
          "bedrock:InvokeModel",
          "bedrock:InvokeModelWithResponseStream",
        ]
        Resource = [
          "arn:aws:bedrock:*::foundation-model/*",
          "arn:aws:bedrock:*:*:inference-profile/*",
        ]
      },
      # bedrock-mantle is its OWN IAM namespace with a project resource, not part of
      # `bedrock:`. The connector target fails to create without ListModels — it discovers
      # the catalog at creation time — and CreateInference is what authorizes the calls.
      # Discovered the hard way: the first target went to FAILED with
      # "not authorized to perform: bedrock-mantle:ListModels on ...:project/default".
      {
        Effect = "Allow"
        Action = [
          "bedrock-mantle:CreateInference",
          "bedrock-mantle:Get*",
          "bedrock-mantle:List*",
        ]
        Resource = "arn:aws:bedrock-mantle:${var.agentcore_gateway_region}:${local.account_id}:project/*"
      },
    ]
  })
}

resource "aws_bedrockagentcore_gateway" "inference" {
  count    = var.agentcore_gateway_enabled ? 1 : 0
  provider = aws.agentcore

  name        = "${local.name}-inference"
  role_arn    = aws_iam_role.agentcore_gateway[0].arn
  description = "AIPlat inference transport - Anthropic Messages dialect over AgentCore Gateway"

  # AWS_IAM inbound auth, so the router signs with the deployment's own credentials
  # (SigV4, service bedrock-agentcore) and no bearer token has to be stored anywhere.
  # CUSTOM_JWT would mean managing a second identity provider for machine-to-machine
  # traffic that already has one.
  authorizer_type = "AWS_IAM"

  depends_on = [aws_iam_role_policy.agentcore_gateway]
}

# Target 1: bedrock-mantle, declared as a PROVIDER target rather than the built-in connector.
#
# The connector is the more obvious choice and it does not work with the trust policy above.
# A connector performs model DISCOVERY at creation time, and that discovery call is made by
# an AWS service-owned principal in another account:
#
#   User: arn:aws:sts::<aws-owned>:assumed-role/GenesisEndpointWorkflowsLambdaRole-.../
#         GatewayWorkflowsLambda is not authorized to perform: sts:AssumeRole on
#         resource: .../aiplat-poc-inf-agentcore-gateway
#
# That path does not satisfy aws:SourceAccount / aws:SourceArn, so the only way to keep a
# connector is to drop the confused-deputy conditions from the execution role's trust policy
# — which would let any gateway the service can reach assume this role. A provider target
# declares its models instead of discovering them, so nothing is assumed at creation and the
# conditions stay. The cost is that this list is maintained by hand; the glob keeps that
# cheap, and an id the endpoint does not serve answers 404 rather than being mis-routed.
resource "aws_bedrockagentcore_gateway_target" "mantle" {
  count    = var.agentcore_gateway_enabled ? 1 : 0
  provider = aws.agentcore

  gateway_identifier = aws_bedrockagentcore_gateway.inference[0].gateway_id
  name               = "mantle"
  description        = "bedrock-mantle Messages API - versionless model ids"

  target_configuration {
    inference {
      provider {
        endpoint = "https://bedrock-mantle.${var.agentcore_gateway_region}.api.aws"

        operation {
          path          = "/v1/messages"
          provider_path = "/anthropic/v1/messages"

          # Bare family ids, no geo prefix: that is the form this endpoint serves, and it is
          # the ONLY place claude-haiku-4-5 is reachable through a gateway — the
          # bedrock-runtime target below rejects it. Without this target the ladder has no
          # cheap rung.
          model {
            model = "anthropic.claude-*"
          }
        }
      }
    }
  }

  credential_provider_configuration {
    gateway_iam_role {}
  }
}

# Target 2: bedrock-runtime, declared explicitly.
#
# provider_path is the part that is easy to get wrong and gives no useful error when it is:
# bedrock-runtime serves the Messages API at /anthropic/v1/messages, and pointing at a bare
# /v1/messages returns HTTP 200 carrying a Coral UnknownOperationException — a success
# status with an error body, which reads as a broken model rather than a wrong path.
resource "aws_bedrockagentcore_gateway_target" "runtime" {
  count    = var.agentcore_gateway_enabled ? 1 : 0
  provider = aws.agentcore

  gateway_identifier = aws_bedrockagentcore_gateway.inference[0].gateway_id
  name               = "runtime"
  description        = "bedrock-runtime Messages API - geo-prefixed inference profiles"

  target_configuration {
    inference {
      provider {
        endpoint = "https://bedrock-runtime.${var.agentcore_gateway_region}.amazonaws.com"

        operation {
          path          = "/v1/messages"
          provider_path = "/anthropic/v1/messages"

          # Globs, not a fixed list: the set of Claude models on this surface changes, and
          # the gateway validates the id against the upstream anyway — an unlisted model
          # answers 404 rather than being silently mis-routed. Only the geo-prefixed forms
          # are declared because a bare `anthropic.claude-*` id is rejected upstream with
          # "Invocation ... with on-demand throughput isn't supported".
          model {
            model = "us.anthropic.claude-*"
          }
          model {
            model = "global.anthropic.claude-*"
          }
        }
      }
    }
  }

  credential_provider_configuration {
    gateway_iam_role {}
  }
}
