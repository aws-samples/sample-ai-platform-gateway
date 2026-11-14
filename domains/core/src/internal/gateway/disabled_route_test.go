// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/aiplat/core/internal/httpapi"
)

// GOV-SEC-010: a route marked disabled MUST NOT be resolvable — naming its alias must be
// unknown_model, and the router must never assume the credential it carries. A placeholder
// BYO row (role_arn, no real model id) marked disabled is the exact case. Fails on old code
// (no Disabled field → the route resolves and the provider/AssumeRole path runs).
func TestDisabledRouteIsNotResolvable(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"_byo_123":{"provider":"bedrock","provider_model_id":"","role_arn":"arn:aws:iam::123456789012:role/AIPlatGatewayAccessX","disabled":true,"capabilities":{"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{}`)

	providerCalled := false
	installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef, _ invocation) (result, error) {
		providerCalled = true
		return result{text: "served"}, nil
	})

	req := httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"_byo_123","messages":[{"role":"user","content":"hi"}]}`,
		RequestID: "test-sec010",
	}
	resp, err := handle(context.Background(), req)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if providerCalled {
		t.Fatal("GOV-SEC-010: a disabled route must never reach the provider (no AssumeRole)")
	}
	if resp.StatusCode != 400 || !strings.Contains(resp.Body, "unknown model") {
		t.Fatalf("naming a disabled route must be an unknown-model 400; got status %d body=%s", resp.StatusCode, resp.Body)
	}
}

// CORE-SEC-005: the derived ExternalID is unguessable when a secret is set (not the literal
// "aiplat-<org>"), and still deterministic. Falls back to the legacy literal when no secret
// is provisioned. Fails on old code (which always returned "aiplat-"+org).
func TestDeriveExternalID(t *testing.T) {
	prev := byoExternalIDSecret
	defer func() { byoExternalIDSecret = prev }()

	byoExternalIDSecret = nil
	if got := deriveExternalID("acme"); got != "aiplat-acme" {
		t.Errorf("no secret: want legacy aiplat-acme, got %q", got)
	}

	byoExternalIDSecret = []byte("a-test-pepper-value-123456")
	got := deriveExternalID("acme")
	if got == "aiplat-acme" {
		t.Errorf("with a secret the ExternalID must NOT be the guessable aiplat-acme")
	}
	if !strings.HasPrefix(got, "aiplat-") {
		t.Errorf("derived ExternalID should keep the aiplat- prefix, got %q", got)
	}
	if again := deriveExternalID("acme"); again != got {
		t.Errorf("derivation must be deterministic: %q != %q", again, got)
	}
	if other := deriveExternalID("other"); other == got {
		t.Errorf("different orgs must derive different ExternalIDs")
	}
}
