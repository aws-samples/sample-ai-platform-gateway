// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package main

import "testing"

// GOV-SEC-002: the role_arn must have the AIPlatGatewayAccess form AND be one the org
// already declares. The regex is the form gate (the account is constrained by the
// allowlist, not the regex).
func TestGatewayRoleARNRe(t *testing.T) {
	ok := []string{
		"arn:aws:iam::387841541630:role/AIPlatGatewayAccess",
		"arn:aws:iam::387841541630:role/AIPlatGatewayAccess-prod",
		"arn:aws:iam::111122223333:role/AIPlatGatewayAccess_x1",
	}
	for _, a := range ok {
		if !gatewayRoleARNRe.MatchString(a) {
			t.Errorf("expected match: %q", a)
		}
	}
	bad := []string{
		"arn:aws:iam::387841541630:role/SomethingElse",
		"arn:aws:iam::12345:role/AIPlatGatewayAccess",            // account not 12 digits
		"arn:aws:iam::387841541630:role/XAIPlatGatewayAccess",    // prefix not anchored
		"role/AIPlatGatewayAccess",                               // not an ARN
		"arn:aws:iam::387841541630:role/AIPlatGatewayAccess/sub", // '/' not allowed in suffix
	}
	for _, a := range bad {
		if gatewayRoleARNRe.MatchString(a) {
			t.Errorf("expected NO match: %q", a)
		}
	}
}

func TestRoleARNDeclaredIn(t *testing.T) {
	routing := map[string]interface{}{
		"m1": map[string]interface{}{"role_arn": "arn:aws:iam::387841541630:role/AIPlatGatewayAccess"},
		"m2": map[string]interface{}{"provider": "bedrock"}, // no role_arn
	}
	if !roleARNDeclaredIn(routing, "arn:aws:iam::387841541630:role/AIPlatGatewayAccess") {
		t.Error("a declared role_arn must be recognised")
	}
	if roleARNDeclaredIn(routing, "arn:aws:iam::111122223333:role/AIPlatGatewayAccess-x") {
		t.Error("an undeclared role_arn must be rejected")
	}
	if roleARNDeclaredIn(nil, "arn:aws:iam::387841541630:role/AIPlatGatewayAccess") {
		t.Error("empty routing declares nothing")
	}
}
