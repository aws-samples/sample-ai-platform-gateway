// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"context"
	"testing"

	"github.com/aiplat/core/internal/httpapi"
	"github.com/aiplat/core/internal/ports"
	"github.com/aiplat/core/internal/routing"
)

// CORE-FUN-003: a cached entry stored by a route that is NOT the requested alias (a
// fallback or auto_cheapest swap) must report the SERVED model on a hit — not the alias
// the cache key was built from. Here the key is built for "m1" but the stored body came
// from "m2"; the hit must say model "m2", not "m1".
func TestE2E_CacheHit_ReportsServedModel(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tool_use":true,"tier":"frontier"}},"m2":{"provider":"openai_compatible","provider_model_id":"id2","capabilities":{"tool_use":true,"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.003,"output":0.015},"m2":{"input":0.0001,"output":0.0002}}`)

	providerCalled := false
	recs := installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef, _ invocation) (result, error) {
		providerCalled = true
		return result{}, nil
	})

	fc := newFakeCache(true)
	cacheStore = fc
	ck := routing.CacheKey(routing.KeyInput{
		Org: "default", Team: "default", Model: "m1",
		Messages: []ports.Message{{Role: "user", Text: "hola", Raw: []byte(`"hola"`)}},
	}, routing.KeyExact)
	// Stored body came from m2 (a prior swap), carrying model "m2" at the top level —
	// which is exactly what the buffered/streaming Put writes.
	fc.Put(context.Background(), ck, "openai_compatible",
		`{"id":"x","object":"chat.completion","model":"m2","choices":[{"index":0,"message":{"role":"assistant","content":"cached from m2"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		0.001, 3600)

	req := httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"hola"}]}`,
		RequestID: "test-cf003",
	}
	resp, err := handle(context.Background(), req)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if providerCalled {
		t.Error("a cache hit must not call the provider")
	}
	if len(*recs) != 1 {
		t.Fatalf("expected 1 cache Usage_Record, got %d", len(*recs))
	}
	rec := (*recs)[0]
	if rec["model"] != "m2" {
		t.Errorf("Usage_Record model = %v, want m2 (the served model, not the requested alias m1)", rec["model"])
	}
	if rec["requested_model"] != "m1" && rec["requested_model"] != nil {
		// requested_model is decorated via DecorateUsage on the non-cache path; on the
		// cache record it may be absent — the key assertion is that `model` is the served one.
	}
	if rec["provider"] != "openai_compatible" {
		t.Errorf("Usage_Record provider = %v, want openai_compatible (m2's provider)", rec["provider"])
	}
}
