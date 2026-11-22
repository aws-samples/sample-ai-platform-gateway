// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aiplat/core/internal/adapters/secrets"
	"github.com/aiplat/core/internal/httpapi"
)

// withSecretStore gives providerFor a secret store when the test runs on its own: the
// package-level store is otherwise only set by e2e_characterization_test, which makes
// `go test -run` on a single test panic depending on order.
func withSecretStore(t *testing.T) {
	t.Helper()
	if secretStore != nil {
		return
	}
	secretStore = secrets.New(nil)
	t.Cleanup(func() { secretStore = nil })
}

// ── the defect these tests exist for ──────────────────────────────────────────
//
// Amazon Bedrock's OpenAI-compatible endpoint selects the cache mode with a top-level BODY
// field, `prompt_cache_options` ({"mode":"implicit"|"explicit","ttl":...}); breakpoints
// travel inside content parts as `prompt_cache_breakpoint`. The gateway kept the
// breakpoints (message content travels through Raw) and dropped the body field, because the
// openai_compatible body is rebuilt from a fixed set of fields. The request still succeeded
// in the provider's default (implicit) mode, which adds an automatic breakpoint on the
// latest message — a cache write on every request — so a caller that asked for explicit
// mode to place only its own breakpoints silently got different caching and billing.

const cacheOpts = `{"mode":"explicit","ttl":"30m"}`

// cachedMsgs is a conversation whose system prompt carries an explicit breakpoint, the shape
// a caller sends when it wants a long stable prefix reused.
func cachedMsgs() []chatMsg {
	return []chatMsg{
		{Role: "system", Content: json.RawMessage(`[{"type":"text","text":"long stable prefix","prompt_cache_breakpoint":{"mode":"explicit"}}]`)},
		{Role: "user", Content: json.RawMessage(`"hi"`)},
	}
}

// captureMsgs is capture with caller-chosen messages: the breakpoint lives inside them.
func captureMsgs(t *testing.T, r Route, msgs []chatMsg, inf invocation) map[string]interface{} {
	t.Helper()
	relaxRouteGuard(t)
	withSecretStore(t)
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ = io.ReadAll(req.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, cannedOpenAI)
	}))
	t.Cleanup(srv.Close)
	r.BaseURL = srv.URL
	if _, err := callProvider(context.Background(), r, msgs, nil, inf); err != nil {
		t.Fatalf("callProvider(%s): %v", r.Provider, err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, string(raw))
	}
	return body
}

func TestPromptCacheOptions_ForwardedOnOptedInRoute(t *testing.T) {
	r := Route{Provider: "openai_compatible", ProviderModelID: "global.openai.gpt-x", PromptCache: true}
	body := captureMsgs(t, r, cachedMsgs(), invocation{PromptCacheOptions: json.RawMessage(cacheOpts)})

	opts, ok := body["prompt_cache_options"].(map[string]interface{})
	if !ok {
		t.Fatalf("prompt_cache_options missing from the wire: %v — the provider never reuses the cached prefix", body)
	}
	if opts["mode"] != "explicit" || opts["ttl"] != "30m" {
		t.Errorf("prompt_cache_options = %v, want the caller's value unchanged", opts)
	}

	// The breakpoint inside the message must survive too: the option alone caches nothing.
	msgs, _ := body["messages"].([]interface{})
	sys, _ := msgs[0].(map[string]interface{})
	parts, _ := sys["content"].([]interface{})
	part, _ := parts[0].(map[string]interface{})
	if _, ok := part["prompt_cache_breakpoint"]; !ok {
		t.Errorf("prompt_cache_breakpoint dropped from the system message: %v", sys)
	}
}

// A route that did not opt in must not receive the option: other OpenAI-compatible upstreams
// (OpenAI itself, Groq, Together) may reject an unknown top-level field with a 400, and the
// operator, not the caller, decides which upstreams get provider-specific fields.
func TestPromptCacheOptions_NotForwardedWithoutOptIn(t *testing.T) {
	r := Route{Provider: "openai_compatible", ProviderModelID: "gpt-x"}
	body := captureMsgs(t, r, cachedMsgs(), invocation{PromptCacheOptions: json.RawMessage(cacheOpts)})
	if v, present := body["prompt_cache_options"]; present {
		t.Errorf("prompt_cache_options = %v reached a route without prompt_cache — must be absent", v)
	}
}

// A request that sends no option keeps the exact wire shape it had before this change.
func TestPromptCacheOptions_AbsentIsNotSent(t *testing.T) {
	r := Route{Provider: "openai_compatible", ProviderModelID: "m", PromptCache: true}
	body := captureMsgs(t, r, cachedMsgs(), invocation{})
	if v, present := body["prompt_cache_options"]; present {
		t.Errorf("prompt_cache_options = %v invented by the gateway — must be absent when the caller sent none", v)
	}
}

// The streaming opener builds its body separately; it must carry the option as well, or a
// stream:true caller silently loses the cache that the buffered path keeps.
func TestPromptCacheOptions_ForwardedOnStream(t *testing.T) {
	relaxRouteGuard(t)
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ = io.ReadAll(req.Body)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	r := Route{Provider: "openai_compatible", BaseURL: srv.URL, ProviderModelID: "m", PromptCache: true}
	inf := invocation{PromptCacheOptions: json.RawMessage(cacheOpts)}
	resp, err := openStreamOpenAICompat(context.Background(), r.BaseURL, r.ProviderModelID, inf.forRoute(r).input(cachedMsgs(), nil), "")
	if err != nil {
		t.Fatalf("openStreamOpenAICompat: %v", err)
	}
	resp.Body.Close()

	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, string(raw))
	}
	if _, ok := body["prompt_cache_options"].(map[string]interface{}); !ok {
		t.Errorf("prompt_cache_options missing from the streaming request: %v", body)
	}
}

// The option must be a JSON object. Anything else is rejected with a named 400 rather than
// forwarded (the provider would 400 with a less useful message) or dropped (which would
// silently bill the full rate again).
func TestPromptCacheOptions_Validation(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantOut string
		wantErr bool
	}{
		{"absent", ``, ``, false},
		{"null", `null`, ``, false},
		{"object", cacheOpts, cacheOpts, false},
		{"string", `"explicit"`, ``, true},
		{"array", `[1]`, ``, true},
		{"number", `1`, ``, true},
	}
	for _, c := range cases {
		out, msg := normalizePromptCacheOptions(json.RawMessage(c.raw))
		if (msg != "") != c.wantErr {
			t.Errorf("%s: error = %q, wantErr %v", c.name, msg, c.wantErr)
		}
		if string(out) != c.wantOut {
			t.Errorf("%s: out = %q, want %q", c.name, string(out), c.wantOut)
		}
	}
}

// ── through the handler ───────────────────────────────────────────────────────
//
// The tests above exercise the pieces. These drive handle() end to end against local
// upstreams, so a regression in the wiring (the body field not parsed, not copied into the
// invocation, or the 400 not returned) fails here even if every piece still works alone.

// upstream is a local OpenAI-compatible server that records every request body it receives
// and answers with the given status and body.
type upstream struct {
	mu     sync.Mutex
	bodies []map[string]interface{}
	srv    *httptest.Server
}

func newUpstream(t *testing.T, status int, ctype, reply string) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var b map[string]interface{}
		_ = json.Unmarshal(raw, &b)
		u.mu.Lock()
		u.bodies = append(u.bodies, b)
		u.mu.Unlock()
		w.Header().Set("content-type", ctype)
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) received() []map[string]interface{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]map[string]interface{}(nil), u.bodies...)
}

// handleWith runs one request through handle() with the real provider path (not the fake
// seam), so the body that reaches the upstream is the one the gateway really builds.
func handleWith(t *testing.T, routing, body string) httpapi.Response {
	t.Helper()
	neutralizeCoreGlobals(t)
	relaxRouteGuard(t)
	t.Setenv("MODEL_ROUTING", routing)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002},"m2":{"input":0.001,"output":0.002}}`)
	installSeams(t, nil)
	callProviderFn = callProvider // installSeams restores the previous value on cleanup
	resp, err := handle(context.Background(), httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      body,
		RequestID: "test-req-prompt-cache-options",
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	return resp
}

func compatRoute(name, baseURL string, promptCache bool, fallback string) string {
	r := map[string]interface{}{
		"provider": "openai_compatible", "provider_model_id": name, "base_url": baseURL,
		"prompt_cache": promptCache, "capabilities": map[string]interface{}{"tool_use": true, "tier": "fast"},
	}
	if fallback != "" {
		r["fallback"] = []string{fallback}
	}
	b, _ := json.Marshal(r)
	return `"` + name + `":` + string(b)
}

const requestWithOpts = `{"model":"m1","messages":[{"role":"system","content":[{"type":"text","text":"long stable prefix","prompt_cache_breakpoint":{"mode":"explicit"}}]},{"role":"user","content":"hi"}],"prompt_cache_options":` + cacheOpts + `}`

func TestPromptCacheOptions_Handler_Buffered(t *testing.T) {
	up := newUpstream(t, 200, "application/json", cannedOpenAI)
	resp := handleWith(t, "{"+compatRoute("m1", up.srv.URL, true, "")+"}", requestWithOpts)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, resp.Body)
	}
	got := up.received()
	if len(got) != 1 {
		t.Fatalf("upstream received %d requests, want 1", len(got))
	}
	if _, ok := got[0]["prompt_cache_options"].(map[string]interface{}); !ok {
		t.Errorf("prompt_cache_options did not reach the upstream through handle(): %v", got[0])
	}
}

func TestPromptCacheOptions_Handler_Stream(t *testing.T) {
	sse := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
	up := newUpstream(t, 200, "text/event-stream", sse)
	body := strings.Replace(requestWithOpts, `{"model":"m1",`, `{"model":"m1","stream":true,`, 1)
	resp := handleWith(t, "{"+compatRoute("m1", up.srv.URL, true, "")+"}", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, resp.Body)
	}
	got := up.received()
	if len(got) != 1 {
		t.Fatalf("upstream received %d requests, want 1", len(got))
	}
	if got[0]["stream"] != true {
		t.Fatalf("expected the streaming path, upstream body: %v", got[0])
	}
	if _, ok := got[0]["prompt_cache_options"].(map[string]interface{}); !ok {
		t.Errorf("prompt_cache_options did not reach the upstream on stream:true: %v", got[0])
	}
}

// A fallback chain can mix an opted-in route with one that is not: the second must not see
// the field even though the same invocation is reused for it.
func TestPromptCacheOptions_Handler_FallbackToRouteWithoutOptIn(t *testing.T) {
	first := newUpstream(t, 503, "application/json", `{"error":{"message":"unavailable"}}`)
	second := newUpstream(t, 200, "application/json", cannedOpenAI)
	routing := "{" + compatRoute("m1", first.srv.URL, true, "m2") + "," + compatRoute("m2", second.srv.URL, false, "") + "}"
	resp := handleWith(t, routing, requestWithOpts)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 from the fallback: %s", resp.StatusCode, resp.Body)
	}
	f, s := first.received(), second.received()
	if len(f) == 0 || len(s) != 1 {
		t.Fatalf("requests: first=%d second=%d, want first>=1 and second=1", len(f), len(s))
	}
	if _, ok := f[0]["prompt_cache_options"]; !ok {
		t.Errorf("the opted-in route did not receive prompt_cache_options: %v", f[0])
	}
	if v, present := s[0]["prompt_cache_options"]; present {
		t.Errorf("prompt_cache_options = %v reached the fallback route without prompt_cache", v)
	}
}

func TestPromptCacheOptions_Handler_RejectsNonObject(t *testing.T) {
	up := newUpstream(t, 200, "application/json", cannedOpenAI)
	body := strings.Replace(requestWithOpts, cacheOpts, `"explicit"`, 1)
	resp := handleWith(t, "{"+compatRoute("m1", up.srv.URL, true, "")+"}", body)
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, resp.Body)
	}
	if !strings.Contains(resp.Body, "prompt_cache_options") {
		t.Errorf("the 400 must name the parameter, got: %s", resp.Body)
	}
	if n := len(up.received()); n != 0 {
		t.Errorf("upstream called %d times for a rejected request, want 0", n)
	}
}
