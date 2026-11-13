// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"os"
	"testing"
)

// TestMain relaxes the route guard for the whole gateway test package: the wire and
// stream tests point base_url at a local http httptest server (loopback, non-https),
// which the production guard rightly rejects. The guard's own logic is covered directly
// by TestValidateRoute below, which calls validateRoute (not the seam).
func TestMain(m *testing.M) {
	validateRouteFn = func(Route) error { return nil }
	os.Exit(m.Run())
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
