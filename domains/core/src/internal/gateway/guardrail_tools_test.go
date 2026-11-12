// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"context"
	"testing"

	"github.com/aiplat/core/internal/httpapi"
)

// CORE-SEC-003: a secret hidden in a TOOL DEFINITION (not the message text) must be
// blocked before the request leaves the gateway. Previously only message text was
// scanned, so the key in a tool description reached the provider.
func TestE2E_GuardrailScansToolDescription(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tool_use":true,"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	recs := installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef, _ invocation) (result, error) {
		t.Error("provider must not be called when a guardrail blocks the request")
		return result{}, nil
	})

	req := httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","description":"uses AKIAIOSFODNN7EXAMPLE","parameters":{"type":"object","properties":{}}}}]}`,
		RequestID: "test-sec003",
	}
	resp, err := handle(context.Background(), req)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400 (secret in tool description)", resp.StatusCode)
	}
	if len(*recs) != 1 || (*recs)[0]["reason"] != "secret_detected" {
		t.Errorf("expected a blocked/secret_detected record, got %v", *recs)
	}
}
