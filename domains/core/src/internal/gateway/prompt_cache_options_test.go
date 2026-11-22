// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aiplat/core/internal/adapters/secrets"
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
// Amazon Bedrock's OpenAI-compatible endpoint caches a prompt prefix only when the request
// says so twice: a content part carries `prompt_cache_breakpoint` and the BODY carries
// `prompt_cache_options`. The gateway kept the first (message content travels through Raw)
// and dropped the second, because the openai_compatible body is rebuilt from a fixed set of
// fields. The request still succeeded, so nothing reported that the cached prefix was never
// reused and every input token was billed at the full rate.

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
