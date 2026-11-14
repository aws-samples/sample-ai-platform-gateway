// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/aiplat/core/internal/httpapi"
	"github.com/aiplat/core/internal/ports"
	"github.com/aiplat/core/internal/routing"
)

// CORE-BND-007: a max_tokens cut must be reported as "length" (the OpenAI dialect) on the
// BUFFERED transport too — not the provider's "max_tokens" vocabulary. Fails on the old
// code, which copied the provider stop reason straight through (finish_reason "max_tokens").
func TestBuffered_MaxTokensFinishReasonIsLength(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef, _ invocation) (result, error) {
		return result{text: "truncated", tin: 3, tout: 4, stopReason: "max_tokens", cacheConv: ports.CacheCountersAbsent}, nil
	})

	req := httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`,
		RequestID: "test-bnd007",
	}
	resp, err := handle(context.Background(), req)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, resp.Body)
	}
	// Dialect-correct finish_reason, not the provider's "max_tokens".
	if !strings.Contains(resp.Body, `"finish_reason":"length"`) {
		t.Errorf("buffered finish_reason must be \"length\" for a max_tokens cut; body=%s", resp.Body)
	}
	if strings.Contains(resp.Body, `"finish_reason":"max_tokens"`) {
		t.Errorf("buffered response leaked the provider vocabulary \"max_tokens\"; body=%s", resp.Body)
	}
}

// CORE-FUN-006: a max_tokens-cut answer cached as "length" must replay as "length" on a
// STREAM hit — not a hardcoded "stop". Fails on the old code, where both cache-replay
// pseudoStream calls passed the literal "stop".
func TestStreamCacheReplay_FinishReasonLength(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	providerCalled := false
	installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef, _ invocation) (result, error) {
		providerCalled = true
		return result{}, nil
	})

	fc := newFakeCache(true)
	cacheStore = fc
	mt := 4
	ck := routing.CacheKey(routing.KeyInput{
		Org: "default", Team: "default", Model: "m1",
		MaxTokens: &mt,
		Messages:  []ports.Message{{Role: "user", Text: "cut me", Raw: []byte(`"cut me"`)}},
	}, routing.KeyExact)
	// A truncated answer stored with the real finish_reason "length" (what the fixed
	// streaming/buffered Put now writes).
	fc.Put(context.Background(), ck, "bedrock",
		`{"id":"x","object":"chat.completion","model":"m1","choices":[{"index":0,"message":{"role":"assistant","content":"trunc"},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
		0.001, 3600)

	req := httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"cut me"}],"max_tokens":4,"stream":true}`,
		RequestID: "test-fun006",
	}
	resp, err := handle(context.Background(), req)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if providerCalled {
		t.Fatal("a cache hit must not call the provider")
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, resp.Body)
	}
	if !strings.Contains(resp.Body, `"finish_reason":"length"`) {
		t.Errorf("stream cache replay must carry finish_reason \"length\"; body=%s", resp.Body)
	}
}

// cachedFinishReason is pure: it reads choices[0].finish_reason, defaulting to "stop".
func TestCachedFinishReason(t *testing.T) {
	cases := []struct {
		in   map[string]interface{}
		want string
	}{
		{map[string]interface{}{"choices": []interface{}{map[string]interface{}{"finish_reason": "length"}}}, "length"},
		{map[string]interface{}{"choices": []interface{}{map[string]interface{}{"finish_reason": "stop"}}}, "stop"},
		{map[string]interface{}{"choices": []interface{}{map[string]interface{}{}}}, "stop"},
		{map[string]interface{}{}, "stop"},
	}
	for i, c := range cases {
		if got := cachedFinishReason(c.in); got != c.want {
			t.Errorf("case %d: cachedFinishReason = %q, want %q", i, got, c.want)
		}
	}
}
