// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Characterization of the HTTP providers' WIRE FORMAT (hexagonal-refactor, task 3 —
// CRITICAL serialization RISK).
//
// Today the call* functions marshal chatMsg straight into the wire format: Content is
// a RawMessage, which preserves multimodal content (an array of parts) and `null` (an
// assistant that only returns tool_calls), besides name/tool_calls/tool_call_id. When
// routing the call through the ports.Message port, the adapter has to REBUILD that
// wire form from Raw (when present) or Text — and it may not change a single byte.
//
// This test drives callProvider (the target of the callProviderFn seam) with an
// httptest server that CAPTURES the body sent to the provider. The captured body
// becomes the golden. It runs BEFORE the extraction (callProvider → call*) and AFTER
// (callProvider → adapter): if a byte changes, the golden catches it. That is the
// safety net for the risk.
//
// Bedrock uses the SDK (Converse), not HTTP with a JSON body inspectable here; its
// preservation is guaranteed by moving the code verbatim.
//
// Regenerate: `go test ./cmd/router -run Wire -update`.
package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// wireMsgs is the representative set that covers the serialization risk: system,
// string, multimodal (array), assistant with content null + tool_calls, and a tool
// message with name + tool_call_id.
func wireMsgs() []chatMsg {
	return []chatMsg{
		{Role: "system", Content: json.RawMessage(`"você é um assistente"`)},
		{Role: "user", Content: json.RawMessage(`"olá"`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"o que é isto?"},{"type":"image_url","image_url":{"url":"data:x"}}]`)},
		{
			Role:    "assistant",
			Content: json.RawMessage(`null`),
			ToolCalls: []toolCall{func() toolCall {
				var tc toolCall
				tc.ID = "call_1"
				tc.Type = "function"
				tc.Function.Name = "get_weather"
				tc.Function.Arguments = `{"city":"SP"}`
				return tc
			}()},
		},
		{Role: "tool", Content: json.RawMessage(`"25C"`), Name: "get_weather", ToolCallID: "call_1"},
	}
}

// captureBody spins up an httptest server, points the route at it, calls callProvider
// and returns the raw body the provider received.
func captureBody(t *testing.T, r Route, cannedResp string) []byte {
	t.Helper()
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		captured, _ = io.ReadAll(req.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, cannedResp)
	}))
	t.Cleanup(srv.Close)
	r.BaseURL = srv.URL

	_, err := callProvider(context.Background(), r, wireMsgs(), nil)
	if err != nil {
		t.Fatalf("callProvider(%s): %v", r.Provider, err)
	}
	return captured
}

// assertWireGolden normalizes the body (reindents the JSON for a stable diff) and
// compares/rewrites the golden.
func assertWireGolden(t *testing.T, name string, body []byte) {
	t.Helper()
	var canon interface{}
	if err := json.Unmarshal(body, &canon); err != nil {
		t.Fatalf("captured body is not valid JSON (%s): %v\n%s", name, err, string(body))
	}
	pretty, _ := json.MarshalIndent(canon, "", "  ")
	path := filepath.Join("testdata", name+".wire.json")

	if *updateGolden {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, append(pretty, '\n'), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		t.Logf("wire golden rewritten: %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("wire golden missing (%s): run with -update. %v", path, err)
	}
	var wantAny interface{}
	json.Unmarshal(want, &wantAny)
	wantCanon, _ := json.Marshal(wantAny)
	gotCanon, _ := json.Marshal(canon)
	if string(wantCanon) != string(gotCanon) {
		t.Errorf("wire form of %q diverged from the golden.\n--- expected ---\n%s\n--- got ---\n%s",
			name, string(want), string(pretty))
	}
}

func TestWire_OpenAICompat(t *testing.T) {
	r := Route{Provider: "openai_compatible", ProviderModelID: "gpt-x"}
	body := captureBody(t, r, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	assertWireGolden(t, "openai", body)
}

func TestWire_Anthropic(t *testing.T) {
	r := Route{Provider: "anthropic", ProviderModelID: "claude-x"}
	body := captureBody(t, r, `{"content":[{"text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	assertWireGolden(t, "anthropic", body)
}

func TestWire_Gemini(t *testing.T) {
	r := Route{Provider: "google", ProviderModelID: "gemini-x"}
	body := captureBody(t, r, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`)
	assertWireGolden(t, "gemini", body)
}
