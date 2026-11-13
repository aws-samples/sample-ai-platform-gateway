// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// HELP-ARC-001: a characterization test over the thin shell (handle) — route dispatch,
// status codes and the role gate. The content logic lives in internal/help (already
// tested); this pins the three routes and the platform_admin gate so a regression in the
// shell cannot ship silently. Audience/client pinning is intentionally out of scope: the
// COGNITO_USER_POOLS authorizer verifies signature + issuer and the pool is single-client,
// which makes audience pinning moot (documented in tf/main.tf).
package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func req(method, path, role string, q map[string]string) events.APIGatewayProxyRequest {
	r := events.APIGatewayProxyRequest{HTTPMethod: method, Path: path, QueryStringParameters: q}
	if role != "" {
		r.RequestContext.Authorizer = map[string]interface{}{
			"claims": map[string]interface{}{"custom:role": role},
		}
	}
	return r
}

func TestHandle_Routes(t *testing.T) {
	ctx := context.Background()

	// FAQ list: any authenticated user, 200 with a tabs array.
	r, _ := handle(ctx, req("GET", "/help/faq", "dev", nil))
	if r.StatusCode != 200 {
		t.Fatalf("faq list: status %d, want 200", r.StatusCode)
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil || body["tabs"] == nil {
		t.Fatalf("faq list: expected a tabs array, got %s", r.Body)
	}

	// Unknown tab → 404.
	if r, _ := handle(ctx, req("GET", "/help/faq", "dev", map[string]string{"tab": "nope"})); r.StatusCode != 404 {
		t.Errorf("unknown tab: status %d, want 404", r.StatusCode)
	}

	// Internal deep-dive denied for a non-platform role.
	if r, _ := handle(ctx, req("GET", "/help/internal", "admin", nil)); r.StatusCode != 403 {
		t.Errorf("internal as admin: status %d, want 403", r.StatusCode)
	}

	// Internal deep-dive allowed for platform_admin (list).
	if r, _ := handle(ctx, req("GET", "/help/internal", "platform_admin", nil)); r.StatusCode != 200 {
		t.Errorf("internal as platform_admin: status %d, want 200", r.StatusCode)
	}

	// OPTIONS preflight → 204.
	if r, _ := handle(ctx, req("OPTIONS", "/help/faq", "", nil)); r.StatusCode != 204 {
		t.Errorf("OPTIONS: status %d, want 204", r.StatusCode)
	}

	// Unknown route → 404.
	if r, _ := handle(ctx, req("GET", "/help/nope", "dev", nil)); r.StatusCode != 404 {
		t.Errorf("unknown route: status %d, want 404", r.StatusCode)
	}
}
