// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

// The SSE frame ORDER is a contract, not a detail: a client stops reading at [DONE], so a
// metadata frame emitted after it is invisible. These tests pin the order for the two
// producers that can reach it — a live provider response and a cache hit — because the
// mistake they guard against is silent. Nothing errors, the stream is still valid SSE, and
// the caller simply never sees what a request cost.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aiplat/core/internal/httpapi"
	"github.com/aiplat/core/internal/ports"
	"github.com/aiplat/core/internal/routing"
)

// sseFrames drains a streaming response and returns the `data:` payloads in order.
func sseFrames(t *testing.T, resp httpapi.Response) []string {
	t.Helper()
	body := resp.Body
	if resp.Stream != nil {
		var sb strings.Builder
		resp.Stream(&sb)
		body = sb.String()
	}
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data:") {
			out = append(out, strings.TrimSpace(line[5:]))
		}
	}
	return out
}

// assertTerminatedByMeta checks the invariant: exactly one [DONE], it is last, and the
// frame immediately before it carries the aiplat block.
func assertTerminatedByMeta(t *testing.T, frames []string) map[string]interface{} {
	t.Helper()
	if len(frames) < 2 {
		t.Fatalf("expected at least a metadata frame and a terminator, got %d frames", len(frames))
	}
	done := 0
	for _, f := range frames {
		if f == "[DONE]" {
			done++
		}
	}
	if done != 1 {
		t.Errorf("expected exactly one [DONE], got %d — a duplicate means a provider terminator was forwarded", done)
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("last frame = %q, expected [DONE]", frames[len(frames)-1])
	}
	var final struct {
		Choices []interface{}          `json:"choices"`
		Usage   map[string]int         `json:"usage"`
		AIPlat  map[string]interface{} `json:"aiplat"`
	}
	if err := json.Unmarshal([]byte(frames[len(frames)-2]), &final); err != nil {
		t.Fatalf("frame before [DONE] is not JSON: %v (%s)", err, frames[len(frames)-2])
	}
	if final.AIPlat == nil {
		t.Fatalf("frame before [DONE] carries no aiplat block: %s", frames[len(frames)-2])
	}
	if len(final.Choices) != 0 {
		t.Errorf("the metadata frame must carry an EMPTY choices array (that is what makes strict OpenAI clients skip it), got %d entries", len(final.Choices))
	}
	if final.Usage == nil {
		t.Error("the metadata frame should carry usage: on providers without native streaming it is the only place a streaming client can get token counts")
	}
	return final.AIPlat
}

// TestStreamEndsWithMetadataThenDone covers the live path: provider answered, tokens and
// cost were computed after the stream was drained, and the figures still reached the
// client.
func TestStreamEndsWithMetadataThenDone(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":1,"output":2}}`)

	installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef) (result, error) {
		return result{text: "hello there", tin: 11, tout: 7}, nil
	})

	resp, err := handle(context.Background(), httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		RequestID: "test-stream-meta",
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, expected 200", resp.StatusCode)
	}
	if got := resp.Headers["content-type"]; !strings.Contains(got, "text/event-stream") {
		t.Errorf("content-type = %q, expected text/event-stream", got)
	}

	frames := sseFrames(t, resp)
	meta := assertTerminatedByMeta(t, frames)

	if meta["cache_hit"] != false {
		t.Errorf("cache_hit = %v, expected false on a provider-served request", meta["cache_hit"])
	}
	if meta["model"] != "m1" {
		t.Errorf("model = %v, expected m1", meta["model"])
	}
	// The cost is the reason this frame exists; a zero here would mean the metadata was
	// assembled before accounting ran.
	if cost, ok := meta["estimated_cost_usd"].(float64); !ok || cost <= 0 {
		t.Errorf("estimated_cost_usd = %v, expected a positive cost (pricing is 1/2 per 1k and 18 tokens were served)", meta["estimated_cost_usd"])
	}

	// The content must still be there: the metadata frame is additive, it does not
	// replace the deltas.
	joined := strings.Join(frames, " ")
	if !strings.Contains(joined, "hello") {
		t.Error("the streamed content is missing from the frames")
	}
}

// fakeStream is a ports.ProviderStream over a fixed list of deltas.
type fakeStream struct {
	deltas []string
	i      int
	res    ports.Result
	closed bool
	// recvAt records how many deltas had been requested when each one was handed over,
	// which is how the test shows the producer is pulling incrementally rather than
	// draining everything first.
	served int
}

func (f *fakeStream) Recv() (string, error) {
	if f.i >= len(f.deltas) {
		return "", io.EOF
	}
	d := f.deltas[f.i]
	f.i++
	f.served++
	return d, nil
}
func (f *fakeStream) Result() ports.Result { return f.res }
func (f *fakeStream) Close() error         { f.closed = true; return nil }

// TestStreamNativeProviderPath drives the ports.StreamProvider path: the handler pulls
// deltas from the provider instead of slicing a finished answer. It also pins the pricing
// consequence — the prompt-cache counters a native stream reports must reach the cost
// model, because this path used to discard them and bill a cache read at the full input
// rate.
func TestStreamNativeProviderPath(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","prompt_cache":true,"capabilities":{"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":10,"output":10,"cache_read":1}}`)

	called := false
	recs := installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef) (result, error) {
		called = true
		return result{text: "buffered fallback"}, nil
	})
	fs := &fakeStream{
		deltas: []string{"one ", "two ", "three"},
		res: ports.Result{
			Text: "one two three", InputTokens: 1000, OutputTokens: 10,
			CacheReadInputTokens: 900, CacheCounters: ports.CacheCountersReported,
			StopReason: "end_turn",
		},
	}
	installStreamSeam(t, func(context.Context, Route, []chatMsg, []toolDef) (ports.ProviderStream, error) {
		return fs, nil
	})

	resp, err := handle(context.Background(), httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		RequestID: "test-stream-native",
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	frames := sseFrames(t, resp)
	meta := assertTerminatedByMeta(t, frames)

	if called {
		t.Error("the buffered call must not run when a native stream opened")
	}
	if fs.served != len(fs.deltas) {
		t.Errorf("served %d of %d deltas — the producer is not draining the provider stream", fs.served, len(fs.deltas))
	}
	if !fs.closed {
		t.Error("the provider stream was not closed; a leaked stream holds the connection until the invocation ends")
	}
	// One frame per delta, not one frame for the whole answer: that is the difference
	// between native streaming and pseudoStream on an already-complete text.
	deltaFrames := 0
	for _, f := range frames {
		if strings.Contains(f, `"content":"one "`) || strings.Contains(f, `"content":"two "`) || strings.Contains(f, `"content":"three"`) {
			deltaFrames++
		}
	}
	if deltaFrames != 3 {
		t.Errorf("got %d delta frames, expected one per provider delta (3)", deltaFrames)
	}

	// Pricing, with prices per 1k tokens and Bedrock's convention that InputTokens
	// EXCLUDES cached tokens (capabilities.cache_tokens_inclusive is false):
	//   1000 uncached input × 10  = 10.0
	//    900 cache read    ×  1  =  0.9
	//     10 output        × 10  =  0.1
	//                        total 11.0
	// Discarding the counters — what this path did before — gives 10.1: the 900 cached
	// tokens are billed as NOTHING. The assertion is exact because every input is fixed;
	// an approximate bound here would pass for both behaviours.
	if cost, ok := meta["estimated_cost_usd"].(float64); !ok || cost != 11.0 {
		t.Errorf("estimated_cost_usd = %v, expected 11.0 (10.0 input + 0.9 cache read + 0.1 output); 10.1 means the cache-read tokens were dropped", meta["estimated_cost_usd"])
	}
	// The other half of the same bug: with no cacheRead there is nothing for
	// routing.CacheSavings to credit, so the prompt cache saved money invisibly.
	// 900 tokens moved from the 10 rate to the 1 rate = 8.1.
	if saved, ok := meta["saved_usd"].(float64); !ok || saved != 8.1 {
		t.Errorf("saved_usd = %v, expected 8.1 from the prompt-cache read", meta["saved_usd"])
	}
	if meta["savings_reason"] != "provider_prompt_cache" {
		t.Errorf("savings_reason = %v, expected provider_prompt_cache", meta["savings_reason"])
	}
	if len(*recs) != 1 {
		t.Fatalf("expected 1 Usage_Record, got %d", len(*recs))
	}
	if got := (*recs)[0]["tokens_out"]; got != float64(10) {
		t.Errorf("tokens_out = %v, expected the provider-reported 10 (not an estimate from the text length)", got)
	}
}

// TestStreamNativeOpenFailureFallsBackSameRoute pins the fallback rule: a provider that
// cannot stream (a Bedrock model without ConverseStream support) must still be served by
// the same route buffered, not skipped as if the route were down.
func TestStreamNativeOpenFailureFallsBackSameRoute(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef) (result, error) {
		return result{text: "served buffered", tin: 5, tout: 3}, nil
	})
	installStreamSeam(t, func(context.Context, Route, []chatMsg, []toolDef) (ports.ProviderStream, error) {
		return nil, errors.New("ValidationException: streaming is not supported for this model")
	})

	resp, err := handle(context.Background(), httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		RequestID: "test-stream-fallback",
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, expected 200 — a model that cannot stream must not fail the request", resp.StatusCode)
	}
	frames := sseFrames(t, resp)
	assertTerminatedByMeta(t, frames)
	if !strings.Contains(strings.Join(frames, " "), "served buffered") {
		t.Error("the buffered answer did not reach the client")
	}
}

// TestStreamCacheHitCarriesMetadata covers the other producer. A cache hit is the request
// most likely to be inspected for cost (it should be zero and report a saving), and it
// takes an entirely different code path from the live one.
func TestStreamCacheHitCarriesMetadata(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	providerCalled := false
	installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef) (result, error) {
		providerCalled = true
		return result{}, nil
	})

	fc := newFakeCache(true)
	cacheStore = fc
	// Team is part of the key: cache_scope defaults to "team" and the fake auth resolves
	// team="default". A key without it would MISS and the test would exercise the live
	// path while looking like a cache test.
	ck := routing.CacheKey(routing.KeyInput{
		Org: "default", Team: "default", Model: "m1",
		Messages: []ports.Message{{Role: "user", Text: "hi", Raw: []byte(`"hi"`)}},
	}, routing.KeyExact)
	fc.Put(context.Background(), ck, "bedrock",
		`{"id":"x","object":"chat.completion","model":"m1","choices":[{"index":0,"message":{"role":"assistant","content":"cached answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		0.02, 3600)

	resp, err := handle(context.Background(), httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		RequestID: "test-stream-cache",
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if providerCalled {
		t.Fatal("a cache hit must not call the provider")
	}

	meta := assertTerminatedByMeta(t, sseFrames(t, resp))
	if meta["cache_hit"] != true {
		t.Errorf("cache_hit = %v, expected true", meta["cache_hit"])
	}
	if saved, ok := meta["saved_usd"].(float64); !ok || saved <= 0 {
		t.Errorf("saved_usd = %v, expected the stored cost to be reported as saved", meta["saved_usd"])
	}
}
