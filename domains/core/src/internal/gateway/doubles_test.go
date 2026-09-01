// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// In-memory doubles of the state ports (hexagonal-refactor, task 4.8).
//
// Every Core port has, besides the production adapter (ddbconfig, ddbcache,
// ddblimits, sqsusage, secrets, ddbkeys), at least one DOUBLE used in tests (R3.2).
// These doubles are in-memory, deterministic and AWS-free — that is what allows
// driving handle() down paths the empty-table adapter cannot reach (e.g. a cache hit)
// and verifying the degradation (Property 9) in a controlled way.
package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aiplat/core/internal/httpapi"
	"github.com/aiplat/core/internal/ports"
	"github.com/aiplat/core/internal/routing"
)

// fakeCache implements ports.Cache in memory.
type fakeCache struct {
	mu      sync.Mutex
	items   map[string]cacheItem
	enabled bool
}
type cacheItem struct {
	json     string
	provider string
	cost     float64
}

func newFakeCache(enabled bool) *fakeCache {
	return &fakeCache{items: map[string]cacheItem{}, enabled: enabled}
}
func (f *fakeCache) Enabled() bool { return f.enabled }
func (f *fakeCache) Get(_ context.Context, key string) (string, float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[key]
	return it.json, it.cost, ok
}
func (f *fakeCache) Put(_ context.Context, key, provider, respJSON string, cost float64, _ int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[key] = cacheItem{json: respJSON, provider: provider, cost: cost}
}

var _ ports.Cache = (*fakeCache)(nil)

// fakeLimits implements ports.LimitsStore in memory (atomic counter).
type fakeLimits struct {
	mu     sync.Mutex
	values map[string]float64
}

func newFakeLimits() *fakeLimits { return &fakeLimits{values: map[string]float64{}} }
func (f *fakeLimits) Bump(_ context.Context, pk, field string, delta float64, _ time.Time) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[pk+"|"+field] += delta
	return f.values[pk+"|"+field], nil
}
func (f *fakeLimits) Read(_ context.Context, pk, field string) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.values[pk+"|"+field]
}

var _ ports.LimitsStore = (*fakeLimits)(nil)

// fakeSecrets implements ports.SecretStore in memory.
type fakeSecrets struct{ byName map[string]string }

func (f *fakeSecrets) Get(_ context.Context, name string) string {
	return f.byName[name]
}

var _ ports.SecretStore = (*fakeSecrets)(nil)

// TestE2E_CacheHit drives handle down the CACHE HIT path using the cache double — a
// path the empty-table adapter (used in the other scenarios) never reaches. It proves
// the handler consumes the port, not the technology.
func TestE2E_CacheHit(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tool_use":true,"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	// Fake auth + usage collector; the provider must NOT be called on a cache hit.
	providerCalled := false
	recs := installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef) (result, error) {
		providerCalled = true
		return result{}, nil
	})

	// Cache double preloaded with the response for the request's cacheKey.
	fc := newFakeCache(true)
	cacheStore = fc
	body := `{"model":"m1","messages":[{"role":"user","content":"oi"}]}`
	ck := routing.CacheKey(routing.KeyInput{
		Org: "default", Model: "m1",
		Messages: []ports.Message{{Role: "user", Text: "oi", Raw: []byte(`"oi"`)}},
	}, routing.KeyExact)
	fc.Put(context.Background(), ck, "bedrock",
		`{"id":"x","object":"chat.completion","model":"m1","choices":[{"index":0,"message":{"role":"assistant","content":"resposta cacheada"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		0.01, 3600)

	req := httpapi.Request{
		Method:    "POST",
		Path:      "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      body,
		RequestID: "test-req-cachehit",
	}
	resp, err := handle(context.Background(), req)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, expected 200", resp.StatusCode)
	}
	if providerCalled {
		t.Error("a cache hit must not call the provider")
	}
	if len(*recs) != 1 {
		t.Fatalf("expected 1 cache Usage_Record, got %d", len(*recs))
	}
	rec := (*recs)[0]
	if rec["cache_hit"] != true {
		t.Errorf("the Usage_Record should mark cache_hit=true, got %v", rec["cache_hit"])
	}
	if rec["savings_reason"] != "cache" {
		t.Errorf("savings_reason = %v, expected cache", rec["savings_reason"])
	}
}
