// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Regression tests for the F-01 audit finding: GET /admin/provider/models puts the org's
// stored provider credential on the wire to a caller-chosen base_url, so it must be gated
// like a credential operation (canAdmin), and the destination must be pinned to a base_url
// the org already declared — never an arbitrary host.
package main

import (
	"context"
	"testing"
)

// The role gate returns BEFORE any AWS call (same pattern as every other case in
// characterization_test.go's TestChar_RouteErrors), so this is safe to run through
// handle() directly even though ddb/secrets are nil in this test binary: reaching either
// would panic, and it does not.
func TestProviderModels_DevIsRefusedBeforeSecretRead(t *testing.T) {
	ctx := context.Background()
	req := jwtReq("GET", "/admin/provider/models", "acme", "dev", "",
		"", map[string]string{"provider": "openai", "base_url": "https://evil.example"})
	out, err := handle(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.StatusCode != 403 {
		t.Fatalf("status=%d, want 403 (body=%s)", out.StatusCode, out.Body)
	}
}

func TestProviderModels_BillingIsRefusedBeforeSecretRead(t *testing.T) {
	ctx := context.Background()
	req := jwtReq("GET", "/admin/provider/models", "acme", "billing", "",
		"", map[string]string{"provider": "openai", "base_url": "https://evil.example"})
	out, err := handle(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.StatusCode != 403 {
		t.Fatalf("status=%d, want 403 (body=%s)", out.StatusCode, out.Body)
	}
}

// baseURLDeclaredIn is the pure allowlist rule pinning the outbound credential-bearing
// call to a destination the org already routes to. Covered directly (no DynamoDB) since
// orgDeclaresBaseURL's only added behavior is reading the routing map and delegating here.
func TestBaseURLDeclaredIn(t *testing.T) {
	routing := map[string]interface{}{
		"gpt-oss": map[string]interface{}{"provider": "openai_compatible", "base_url": "https://api.openai.com/v1"},
	}
	cases := []struct {
		name    string
		routing map[string]interface{}
		base    string
		want    bool
	}{
		{"exact match", routing, "https://api.openai.com/v1", true},
		{"trailing slash ignored", routing, "https://api.openai.com/v1/", true},
		{"case insensitive", routing, "HTTPS://API.OPENAI.COM/v1", true},
		{"undeclared host is refused", routing, "https://evil.example", false},
		{"no routing at all", nil, "https://api.openai.com/v1", false},
		{"empty routing map", map[string]interface{}{}, "https://api.openai.com/v1", false},
		{"a route with no base_url does not match empty input", routing, "", false},
	}
	for _, c := range cases {
		if got := baseURLDeclaredIn(c.routing, c.base); got != c.want {
			t.Errorf("%s: baseURLDeclaredIn(_, %q) = %v, want %v", c.name, c.base, got, c.want)
		}
	}
}
