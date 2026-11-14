// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"context"
	"testing"
)

// relaxRouteGuard disables the data-plane route guard for ONE test (restored on cleanup).
// CORE-SEC-005: the guard is NO LONGER disabled package-wide in a TestMain — a global
// nil-guard meant no handler/e2e test ever ran the real guard, so a regression that
// stopped calling it would not fail any test. Only the wire/stream tests that deliberately
// point base_url at a loopback httptest server opt out, locally and explicitly, so every
// other test (including the e2e streaming paths) exercises the real validateRoute.
func relaxRouteGuard(t *testing.T) {
	t.Helper()
	prev := validateRouteFn
	validateRouteFn = func(Route) error { return nil }
	t.Cleanup(func() { validateRouteFn = prev })
}

// TestProviderFor_GuardsRouteByDefault is the handler-adjacent proof the guard actually
// runs: with the DEFAULT seam (no relaxation) providerFor must reject a loopback base_url.
// It fails if a refactor ever drops the validateRouteFn call from providerFor, or if the
// guard is globally disabled again.
func TestProviderFor_GuardsRouteByDefault(t *testing.T) {
	_, err := providerFor(context.Background(), Route{Provider: "openai_compatible", BaseURL: "https://127.0.0.1/v1"})
	if err == nil {
		t.Fatal("CORE-SEC-005: providerFor must reject a loopback base_url under the real guard")
	}
}

// CORE-SEC-005: the data-plane route guard rejects a non-https or non-routable base_url,
// a role_arn outside the AIPlatGatewayAccess* allowlist, and a derived external_id when
// the deployment org is unset.
func TestValidateRoute(t *testing.T) {
	good := []Route{
		{Provider: "openai_compatible", BaseURL: "https://api.openai.com/v1"},
		{Provider: "anthropic"}, // empty base_url uses the default endpoint
		{Provider: "bedrock", RoleARN: "arn:aws:iam::123456789012:role/AIPlatGatewayAccessXYZ", ExternalID: "aiplat-acme"},
	}
	for _, r := range good {
		if err := validateRoute(r); err != nil {
			t.Errorf("expected %+v to pass, got %v", r, err)
		}
	}

	bad := []struct {
		name string
		r    Route
	}{
		{"http scheme", Route{BaseURL: "http://api.example.com"}},
		{"IMDS link-local", Route{BaseURL: "https://169.254.169.254/latest/meta-data/"}},
		{"loopback", Route{BaseURL: "https://127.0.0.1/x"}},
		{"private", Route{BaseURL: "https://10.1.2.3/x"}},
		{"localhost", Route{BaseURL: "https://localhost/x"}},
		{"role_arn not allowlisted", Route{Provider: "bedrock", RoleARN: "arn:aws:iam::123456789012:role/SomethingElse", ExternalID: "x"}},
	}
	for _, tc := range bad {
		if err := validateRoute(tc.r); err == nil {
			t.Errorf("%s: expected rejection, got nil", tc.name)
		}
	}

	// Confused-deputy: a role_arn with no explicit external_id and the default deployment
	// org must be refused.
	prev := deploymentOrg
	deploymentOrg = "default"
	defer func() { deploymentOrg = prev }()
	if err := validateRoute(Route{Provider: "bedrock", RoleARN: "arn:aws:iam::123456789012:role/AIPlatGatewayAccess"}); err == nil {
		t.Error("expected refusal: derived external_id with unset deployment org")
	}
}
