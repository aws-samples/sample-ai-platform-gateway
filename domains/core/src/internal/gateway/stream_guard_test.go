// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"context"
	"testing"

	"github.com/aiplat/core/internal/httpapi"
	"github.com/aiplat/core/internal/ports"
)

// CORE-SEC-005: the STREAMING path must run the data-plane route guard BEFORE it opens any
// upstream — a route with a non-https/loopback base_url or a non-allowlisted role_arn must
// advance the chain without a provider call. The round-2 test only covered providerFor (the
// buffered path), so deleting the streaming guard left the package green. This drives a
// STREAM request against a bedrock route whose role_arn is NOT an AIPlatGatewayAccess role
// and asserts the native-stream opener is never reached. Deleting the guard at the top of
// the streaming loop makes the opener fire → this test fails.
func TestStreamingRouteGuardAdvancesBeforeUpstream(t *testing.T) {
	neutralizeCoreGlobals(t)
	// A single bedrock route with a role_arn the guard rejects (not AIPlatGatewayAccess*).
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","role_arn":"arn:aws:iam::123456789012:role/SomethingElse","external_id":"x","capabilities":{"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef, _ invocation) (result, error) {
		return result{text: "should not be served", tin: 1, tout: 1}, nil
	})
	streamOpened := false
	installStreamSeam(t, func(context.Context, Route, []chatMsg, []toolDef, invocation) (ports.ProviderStream, error) {
		streamOpened = true
		return nil, errNoNativeStream
	})

	req := httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		RequestID: "test-sec005",
	}
	resp, err := handle(context.Background(), req)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if streamOpened {
		t.Fatal("CORE-SEC-005: the streaming path opened an upstream for a guard-rejected route (role_arn not allowlisted); the guard did not run before the upstream call")
	}
	// The whole (single-route) chain is rejected, so the request cannot be served.
	if resp.StatusCode == 200 {
		t.Fatalf("a guard-rejected route must not produce a 200; got status %d", resp.StatusCode)
	}
}
