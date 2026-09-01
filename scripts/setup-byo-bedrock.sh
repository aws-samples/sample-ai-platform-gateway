#!/bin/bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
# =============================================================================
# Setup BYO Bedrock — configures YOUR AWS account so AIPlat can invoke Bedrock
# in it (cross-account), billing your account instead of the platform's.
#
# It creates a single IAM role in your account with:
#   - a trust policy scoped to the platform's GATEWAY ROLE (not the whole
#     account) plus a per-org External ID (confused-deputy mitigation);
#   - permission to invoke Bedrock models only.
# =============================================================================
set -e

echo "╔═══════════════════════════════════════════════════════════════════════╗"
echo "║           AIPlat - Setup BYO Bedrock (Bring Your Own)                 ║"
echo "╚═══════════════════════════════════════════════════════════════════════╝"
echo ""

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

# AIPlat platform account (the one that assumes the role). Set it to your
# platform's real account ID — the console shows it as "Platform account".
AIPLAT_ACCOUNT="${AIPLAT_ACCOUNT:-111122223333}"
# Name of the platform's gateway execution role, which is the ONLY principal
# allowed to assume the role created here. It follows the project naming
# convention: <project>-<environment>-inf-router.
AIPLAT_GATEWAY_ROLE="${AIPLAT_GATEWAY_ROLE:-aiplat-poc-inf-router}"
ROLE_NAME="AIPlatGatewayAccess"

# Detect your AWS account
YOUR_ACCOUNT=$(aws sts get-caller-identity --query Account --output text 2>/dev/null)
if [ -z "$YOUR_ACCOUNT" ]; then
    echo "❌ Error: could not detect your AWS account. Check your credentials."
    exit 1
fi

echo -e "${CYAN}📋 Your AWS account:${NC} $YOUR_ACCOUNT"
echo -e "${CYAN}📋 Platform principal:${NC} arn:aws:iam::${AIPLAT_ACCOUNT}:role/${AIPLAT_GATEWAY_ROLE}"
echo ""

# Ask for the org_id (you get it after creating your account in the console)
read -p "🏢 What is your AIPlat org_id? (e.g. org_abc123, or leave empty to generate one): " ORG_ID

if [ -z "$ORG_ID" ]; then
    # 128 bits: the org id is a public-ish identifier, but it should still not be
    # enumerable by walking a small keyspace.
    ORG_ID="org_$(openssl rand -hex 16)"
    echo -e "${YELLOW}⚡ Generated Org ID:${NC} $ORG_ID"
fi

# The External ID is the confused-deputy control: it is the only thing that stops
# the platform from being tricked into using its own trusted principal to reach a
# role it was not meant to reach. It must therefore be UNGUESSABLE. Deriving it
# purely from the org id would break that, because the org id is visible in the
# console and can be shared or leaked without anyone treating it as a secret.
# So the External ID carries its own 128-bit random component that never appears
# anywhere the customer reads an org id from.
EXTERNAL_ID="aiplat-${ORG_ID}-$(openssl rand -hex 16)"

# Bedrock region
read -p "🌎 Bedrock region (default: us-east-1): " BEDROCK_REGION
BEDROCK_REGION=${BEDROCK_REGION:-us-east-1}

echo ""
echo -e "${CYAN}══════════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}🔧 Creating IAM role: ${ROLE_NAME}${NC}"
echo -e "${CYAN}══════════════════════════════════════════════════════════════════${NC}"

# Trust policy: only the platform's gateway ROLE may assume this role, and only
# when it presents your org's External ID. Trusting the platform account root
# instead would let ANY principal in that account assume it.
TRUST_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "arn:aws:iam::${AIPLAT_ACCOUNT}:role/${AIPLAT_GATEWAY_ROLE}"
      },
      "Action": "sts:AssumeRole",
      "Condition": {
        "StringEquals": {
          "sts:ExternalId": "${EXTERNAL_ID}"
        }
      }
    }
  ]
}
EOF
)

# Create the role (or update it if it already exists)
if aws iam get-role --role-name "$ROLE_NAME" >/dev/null 2>&1; then
    echo "📝 Role already exists, updating trust policy..."
    aws iam update-assume-role-policy \
        --role-name "$ROLE_NAME" \
        --policy-document "$TRUST_POLICY"
else
    echo "📝 Creating role..."
    aws iam create-role \
        --role-name "$ROLE_NAME" \
        --assume-role-policy-document "$TRUST_POLICY" \
        --description "Allows the AIPlat gateway to invoke Bedrock in this account" \
        --tags Key=ManagedBy,Value=aiplat Key=Purpose,Value=byo-bedrock
fi

# Bedrock permission policy. Scoped to model resources instead of "*": the
# gateway only invokes foundation models (and cross-region inference profiles).
BEDROCK_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "BedrockInvoke",
      "Effect": "Allow",
      "Action": [
        "bedrock:InvokeModel",
        "bedrock:InvokeModelWithResponseStream"
      ],
      "Resource": [
        "arn:aws:bedrock:*::foundation-model/*",
        "arn:aws:bedrock:*:${YOUR_ACCOUNT}:inference-profile/*"
      ]
    }
  ]
}
EOF
)

# Attach inline policy
echo "📝 Configuring Bedrock permissions..."
aws iam put-role-policy \
    --role-name "$ROLE_NAME" \
    --policy-name "BedrockInvoke" \
    --policy-document "$BEDROCK_POLICY"

ROLE_ARN="arn:aws:iam::${YOUR_ACCOUNT}:role/${ROLE_NAME}"

echo ""
echo -e "${GREEN}✅ Role created successfully!${NC}"
echo ""

# Output for the console
echo -e "${CYAN}══════════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}📋 COPY THESE SETTINGS INTO THE AIPLAT CONSOLE:${NC}"
echo -e "${CYAN}══════════════════════════════════════════════════════════════════${NC}"
echo ""
echo -e "${YELLOW}bedrock_role_arn:${NC}      $ROLE_ARN"
echo -e "${YELLOW}bedrock_external_id:${NC}   $EXTERNAL_ID"
echo -e "${YELLOW}bedrock_region:${NC}        $BEDROCK_REGION"
echo ""

# JSON for PUT /admin/config
CONFIG_JSON=$(cat <<EOF
{
  "bedrock_role_arn": "${ROLE_ARN}",
  "bedrock_external_id": "${EXTERNAL_ID}",
  "bedrock_region": "${BEDROCK_REGION}"
}
EOF
)

echo -e "${CYAN}══════════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}📝 JSON to configure it via the API (after signing in):${NC}"
echo -e "${CYAN}══════════════════════════════════════════════════════════════════${NC}"
echo ""
echo "$CONFIG_JSON"
echo ""

# Save to a file for convenience
CONFIG_FILE="/tmp/aiplat-byo-bedrock-config.json"
echo "$CONFIG_JSON" > "$CONFIG_FILE"
echo -e "${GREEN}💾 Config saved to:${NC} $CONFIG_FILE"
echo ""

# Ready-to-run curl
echo -e "${CYAN}══════════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}🚀 Command to apply it (replace <YOUR_JWT>):${NC}"
echo -e "${CYAN}══════════════════════════════════════════════════════════════════${NC}"
echo ""
cat <<EOF
curl -X PUT "https://<admin-api-id>.execute-api.<region>.amazonaws.com/admin/config" \\
  -H "Authorization: Bearer <YOUR_JWT>" \\
  -H "Content-Type: application/json" \\
  -d '$CONFIG_JSON'
EOF
echo ""

# Quick test
echo -e "${CYAN}══════════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}🧪 Quick test (after configuring, replace <API_KEY>):${NC}"
echo -e "${CYAN}══════════════════════════════════════════════════════════════════${NC}"
echo ""
cat <<EOF
curl -X POST "https://<gateway-id>.execute-api.<region>.amazonaws.com/v1/chat/completions" \\
  -H "Authorization: Bearer <API_KEY>" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "bedrock/anthropic.claude-3-haiku-20240307-v1:0",
    "messages": [{"role": "user", "content": "Say hi!"}]
  }'
EOF
echo ""

echo -e "${GREEN}══════════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}✅ Setup complete! Next:${NC}"
echo -e "${GREEN}   1. Create your account at: https://<your-console>.cloudfront.net${NC}"
echo -e "${GREEN}   2. Note your org_id (shown in the console after sign-in)${NC}"
echo -e "${GREEN}   3. Configure BYO Bedrock in Settings or via the curl above${NC}"
echo -e "${GREEN}   4. Create an API Key and test!${NC}"
echo -e "${GREEN}══════════════════════════════════════════════════════════════════${NC}"
