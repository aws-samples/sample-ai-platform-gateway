// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/aiplat/core/internal/httpapi"
)

func TestValidFeature(t *testing.T) {
	ok := []string{"", "chat", "code_review", "a.b-c_1", strings.Repeat("x", 64)}
	for _, f := range ok {
		if !validFeature(f) {
			t.Errorf("validFeature(%q) = false, want true", f)
		}
	}
	bad := []string{strings.Repeat("x", 65), "has space", "slash/here", "emoji😀", "semi;colon"}
	for _, f := range bad {
		if validFeature(f) {
			t.Errorf("validFeature(%q) = true, want false", f)
		}
	}
}

// CORE-NFR-002: an oversized feature must be rejected up front (400), so it never enters
// the Usage_Record / SQS message (which the 256 KB limit would silently drop while the
// request still returned 200). The failure is recorded as a client 4xx.
func TestE2E_OversizedFeatureRejected(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tool_use":true,"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	providerCalled := false
	recs := installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef, _ invocation) (result, error) {
		providerCalled = true
		return result{text: "ok", tin: 1, tout: 1}, nil
	})

	huge := strings.Repeat("f", 300000)
	req := httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"hi"}],"feature":"` + huge + `"}`,
		RequestID: "test-nfr002",
	}
	resp, err := handle(context.Background(), req)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400 for an oversized feature", resp.StatusCode)
	}
	if providerCalled {
		t.Error("an oversized feature must be rejected before the provider is called")
	}
	if len(*recs) != 1 {
		t.Fatalf("expected exactly 1 (blocked) Usage_Record, got %d", len(*recs))
	}
	rec := (*recs)[0]
	if rec["reason"] != "invalid_request" || rec["status"] != "blocked" {
		t.Errorf("record = reason %v / status %v, want invalid_request/blocked", rec["reason"], rec["status"])
	}
	// The huge value must NOT have been carried into the record.
	if f, _ := rec["feature"].(string); len(f) > 64 {
		t.Errorf("the oversized feature leaked into the Usage_Record (len=%d)", len(f))
	}
}
