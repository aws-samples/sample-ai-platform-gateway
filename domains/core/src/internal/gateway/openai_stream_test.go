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

	"github.com/aiplat/core/internal/ports"
)

// CORE-BND-001: the openai_compatible streaming opener must send the SAME body as the
// buffered call — max_tokens, temperature, top_p, stop, reasoning_effort and tools — not a
// stripped model+messages body. Asserted against an httptest upstream capturing the body.
func TestOpenStreamOpenAICompat_ForwardsParamsAndTools(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	temp, top := 0.3, 0.9
	in := ports.InvokeInput{
		Messages:        []ports.Message{{Role: "user", Text: "hi", Raw: []byte(`"hi"`)}},
		Tools:           []ports.ToolDef{{Name: "get_weather", Parameters: map[string]interface{}{"type": "object"}}},
		MaxOutputTokens: 50,
		Inference: ports.InferenceParams{
			Temperature: &temp, TopP: &top, Stop: []string{"END"},
			Reasoning: &ports.ReasoningRequest{Effort: "low"},
		},
	}
	resp, err := openStreamOpenAICompat(context.Background(), srv.URL, "m-id", in, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	resp.Body.Close()

	var body map[string]interface{}
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("bad body: %v (%s)", err, got)
	}
	if body["stream"] != true {
		t.Errorf("stream flag missing")
	}
	if body["max_tokens"] != float64(50) {
		t.Errorf("max_tokens = %v, want 50", body["max_tokens"])
	}
	if body["temperature"] != 0.3 {
		t.Errorf("temperature = %v, want 0.3", body["temperature"])
	}
	if body["top_p"] != 0.9 {
		t.Errorf("top_p = %v, want 0.9", body["top_p"])
	}
	if body["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort = %v, want low", body["reasoning_effort"])
	}
	if _, ok := body["stop"]; !ok {
		t.Errorf("stop sequences missing")
	}
	tools, ok := body["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want 1 tool", body["tools"])
	}
}
