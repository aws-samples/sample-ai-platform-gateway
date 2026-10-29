# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Amazon Bedrock AgentCore Gateway — OPTIONAL transport, backing two router providers:
#   `bedrock_gateway`  — the Anthropic Messages dialect (targets `mantle` and `runtime`).
#   `bedrock_invoke`   — the native InvokeModel API (target `invoke`), which reaches the
#                        families the Messages dialect cannot (see "WHAT THIS CAN NOW
#                        REACH" below).
#
# Everything here is gated on var.agentcore_gateway_enabled and defaults to OFF; the native
# InvokeModel target is gated additionally on var.agentcore_invoke_enabled, also OFF by
# default. The gateway is an alternative door to the same models the `bedrock` provider
# already reaches directly, so it is a comparison and a choice, not a dependency: a
# deployment that never sets the flags is byte-identical to one without this file.
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
# WHAT THIS CAN NOW REACH, and what it still cannot
#
# The Messages dialect (targets `mantle`/`runtime`, provider `bedrock_gateway`) accepts
# /v1/messages, /v1/chat/completions and /v1/responses and REJECTS a model id containing
# ':'. Every versioned Bedrock id ends in "-v1:0", so through that provider Amazon Nova,
# Meta Llama and the older Claude generations are unreachable.
#
# The native InvokeModel target (`invoke`, provider `bedrock_invoke`) lifts that limit for
# the non-Claude families: the id travels in the URL path (/model/{model}/invoke), so a ':'
# is a path segment, not a rejected body field. MEASURED 2026-10-26 (batch B1,
# docs/agentcore-gateway-capabilities.md): us.amazon.nova-micro-v1:0 returns HTTP 200
# through this target. The model globs below declare Amazon Nova and Meta Llama; the
# `bedrock_invoke` adapter currently maps only the Amazon Nova native schema (Llama's
# InvokeModel body differs and is a later increment).
#
# STILL unreached by any gateway transport, and kept on provider "bedrock":
#   - Cohere / Titan embeddings (no gateway inference path for them here).
#   - Meta Llama until the `bedrock_invoke` adapter maps its native schema.
#   - Any route that wants the direct-to-Bedrock path for latency or cost comparison —
#     every family above is still reachable directly, so the gateway stays a choice.

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

# Target 3: bedrock-runtime, NATIVE InvokeModel API. OPTIONAL on top of the gateway, gated
# by var.agentcore_invoke_enabled (which also requires agentcore_gateway_enabled).
#
# WHY THIS TARGET EXISTS
#
# The Messages targets above reach only the Anthropic Claude family, because the gateway's
# Messages path rejects a model id containing ':'. Amazon Nova, Meta Llama and the versioned
# Bedrock ids (ending "-v1:0") are unreachable there. AWS added InvokeModel inference targets
# on 2026-10-22, where the model id travels in the URL path (/model/{model}/invoke) instead
# of the request body, so a ':' in the id is a path segment, not a rejected body field.
# MEASURED 2026-10-26 (batch B1, docs/agentcore-gateway-capabilities.md): the ':' does pass
# here — us.amazon.nova-micro-v1:0 returns HTTP 200 through this target.
#
# WHY A PROVIDER TARGET, NOT THE bedrock-runtime CONNECTOR
#
# Same reason as the mantle target: the connector performs model discovery at creation time
# as an AWS service-owned principal, which does not satisfy the aws:SourceAccount /
# aws:SourceArn conditions on the execution role's trust policy. Keeping a provider target
# (models declared, nothing discovered) preserves the confused-deputy mitigation. A declared
# glob the endpoint does not serve answers 404 at call time rather than being mis-routed.
#
# IAM: InvokeModel plus a model-discovery grant.
#
# The execution role already grants bedrock:InvokeModel and bedrock:InvokeModelWithResponseStream
# on foundation-model/* and inference-profile/*, which is what the native InvokeModel path
# calls. But a bedrock-runtime InvokeModel target ALSO discovers its model catalog at
# creation. MEASURED 2026-10-26: without bedrock:ListFoundationModels the target goes to
# FAILED with "Failed to discover models from inference provider ... not authorized to
# perform: bedrock:ListFoundationModels". Unlike the connector, that discovery call is made
# AS THIS EXECUTION ROLE (the error principal is
# assumed-role/aiplat-poc-inf-agentcore-gateway/inference-iam-auth-session), not an
# AWS-owned principal, so granting the list actions does NOT require weakening the
# confused-deputy trust conditions. The grant is in aws_iam_role_policy.agentcore_invoke_discovery
# below, gated by the same flag.
resource "aws_bedrockagentcore_gateway_target" "invoke" {
  count    = var.agentcore_gateway_enabled && var.agentcore_invoke_enabled ? 1 : 0
  provider = aws.agentcore

  gateway_identifier = aws_bedrockagentcore_gateway.inference[0].gateway_id
  name               = "invoke"
  description        = "bedrock-runtime native InvokeModel API - Nova, Llama, versioned ids"

  target_configuration {
    inference {
      provider {
        endpoint = "https://bedrock-runtime.${var.agentcore_gateway_region}.amazonaws.com"

        # Native InvokeModel. No provider_path: this is passthrough to the same path on the
        # Bedrock Runtime endpoint, not a dialect translation. The model globs cover the
        # families the Messages targets cannot serve. They are declared, not discovered, and
        # the gateway validates the id against the upstream at call time.
        operation {
          path = "/model/{model}/invoke"
          model { model = "amazon.nova-*" }
          model { model = "us.amazon.nova-*" }
          model { model = "meta.llama*" }
          model { model = "us.meta.llama*" }
        }
        operation {
          path = "/model/{model}/invoke-with-response-stream"
          model { model = "amazon.nova-*" }
          model { model = "us.amazon.nova-*" }
          model { model = "meta.llama*" }
          model { model = "us.meta.llama*" }
        }
      }
    }
  }

  credential_provider_configuration {
    gateway_iam_role {}
  }

  # The discovery grant must exist before this target is created, or the catalog scan at
  # creation fails with a 403 and the target goes to FAILED.
  depends_on = [aws_iam_role_policy.agentcore_invoke_discovery]
}

# Model-discovery grant for the native InvokeModel target (target 3 above).
#
# A bedrock-runtime InvokeModel target scans the Amazon Bedrock model catalog when it is
# created, and the scan runs AS this execution role. Without the list actions the target
# never reaches READY (MEASURED 2026-10-26, see the target comment). The set below is the
# one the AWS InvokeModel-target doc names. A list API that a region does not offer is
# skipped during discovery, so granting the full set is safe in every region. Resource is
# "*" because list calls are not resource-addressable, the same pattern the governance
# role already uses for bedrock:ListFoundationModels.
resource "aws_iam_role_policy" "agentcore_invoke_discovery" {
  count = var.agentcore_gateway_enabled && var.agentcore_invoke_enabled ? 1 : 0
  name  = "invoke-discovery"
  role  = aws_iam_role.agentcore_gateway[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "bedrock:ListFoundationModels",
          "bedrock:ListInferenceProfiles",
          "bedrock:ListProvisionedModelThroughputs",
          "bedrock:ListCustomModelDeployments",
          "bedrock:ListImportedModels",
          "bedrock:ListMarketplaceModelEndpoints",
          "bedrock:ListPromptRouters",
        ]
        Resource = "*"
      },
    ]
  })
}
