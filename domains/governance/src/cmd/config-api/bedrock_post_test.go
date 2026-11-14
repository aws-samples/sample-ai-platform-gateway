// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// GOV-SEC-010: the request-scoped POST /admin/bedrock/models replaces the placeholder-route
// hack. It must keep the AssumeRole-oracle gates — owner/admin only, and the ARN must have
// the AIPlatGatewayAccess* FORM — and it must NOT require the ARN to be declared on a route
// (so first-time onboarding can list models) nor write any config. These cases return BEFORE
// the STS/Bedrock call, so they run offline. Fails on the old code (no POST route → 404).
func TestBedrockModelsPOST_Gates(t *testing.T) {
	ctx := context.Background()

	// A dev may not list.
	out, _ := handle(ctx, jwtReq("POST", "/admin/bedrock/models", "acme", "dev", "", `{"role_arn":"arn:aws:iam::123456789012:role/AIPlatGatewayAccess"}`, nil))
	if out.StatusCode != 403 {
		t.Fatalf("dev POST bedrock/models: status=%d, want 403 (body=%s)", out.StatusCode, out.Body)
	}

	// Owner with a non-AIPlatGatewayAccess ARN → 400 form error (before any STS call).
	out, _ = handle(ctx, jwtReq("POST", "/admin/bedrock/models", "acme", "owner", "", `{"role_arn":"arn:aws:iam::123456789012:role/SomethingElse"}`, nil))
	if out.StatusCode != 400 {
		t.Fatalf("owner POST bad-form ARN: status=%d, want 400 (body=%s)", out.StatusCode, out.Body)
	}
	var m map[string]interface{}
	json.Unmarshal([]byte(out.Body), &m)
	if es, _ := m["error"].(string); !strings.Contains(es, "AIPlatGatewayAccess") {
		t.Fatalf("expected an AIPlatGatewayAccess form error, got %q", es)
	}

	// Owner with no role_arn → 400 required.
	out, _ = handle(ctx, jwtReq("POST", "/admin/bedrock/models", "acme", "owner", "", `{}`, nil))
	if out.StatusCode != 400 {
		t.Fatalf("owner POST no ARN: status=%d, want 400", out.StatusCode)
	}
}
