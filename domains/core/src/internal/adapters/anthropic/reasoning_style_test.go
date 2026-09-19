// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aiplat/core/internal/ports"
)

// captureThinking sends one request and returns the `thinking` and `output_config` the
// adapter put on the wire.
func captureThinking(t *testing.T, style string, r *ports.ReasoningRequest) map[string]interface{} {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ = io.ReadAll(req.Body)
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)

	a := &Adapter{HTTP: srv.Client(), BaseURL: srv.URL, ModelID: "m",
		Transport: Transport{ReasoningStyle: style}}
	if _, err := a.Invoke(context.Background(), ports.InvokeInput{
		Messages:  []ports.Message{{Role: "user", Text: "hi"}},
		Inference: ports.InferenceParams{Reasoning: r},
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var sent map[string]interface{}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	return sent
}

// TestReasoningStyle_BudgetIsTheDefault pins the original shape. A route that says nothing
// must keep behaving exactly as before — this is the shape claude-haiku-4-5 and Anthropic's
// own API accept, and the ONLY one they accept.
func TestReasoningStyle_BudgetIsTheDefault(t *testing.T) {
	for _, style := range []string{"", ReasoningStyleBudget} {
		sent := captureThinking(t, style, &ports.ReasoningRequest{Effort: "high", BudgetTokens: 4096})
		th, ok := sent["thinking"].(map[string]interface{})
		if !ok {
			t.Fatalf("style %q: no thinking field: %v", style, sent)
		}
		if th["type"] != "enabled" {
			t.Errorf("style %q: thinking.type = %v, want enabled", style, th["type"])
		}
		if th["budget_tokens"] != float64(4096) {
			t.Errorf("style %q: budget_tokens = %v, want 4096", style, th["budget_tokens"])
		}
		// output_config belongs to the adaptive shape only; haiku-4-5 answers
		// "This model does not support the effort parameter."
		if _, present := sent["output_config"]; present {
			t.Errorf("style %q: output_config must not be sent with the budget shape", style)
		}
	}
}

// TestReasoningStyle_AdaptiveSendsEffortNotBudget covers the shape sonnet-5 / opus-4-8
// require. Sending budget_tokens to them is a hard 400, so the budget must be DROPPED, not
// merely accompanied by the new fields.
func TestReasoningStyle_AdaptiveSendsEffortNotBudget(t *testing.T) {
	sent := captureThinking(t, ReasoningStyleAdaptive,
		&ports.ReasoningRequest{Effort: ports.ReasoningEffortLow, BudgetTokens: 1024})

	th, ok := sent["thinking"].(map[string]interface{})
	if !ok {
		t.Fatalf("no thinking field: %v", sent)
	}
	if th["type"] != "adaptive" {
		t.Errorf("thinking.type = %v, want adaptive", th["type"])
	}
	if _, present := th["budget_tokens"]; present {
		t.Error("budget_tokens must NOT be sent with the adaptive shape; the model rejects it")
	}
	oc, ok := sent["output_config"].(map[string]interface{})
	if !ok {
		t.Fatalf("no output_config: %v", sent)
	}
	if oc["effort"] != "low" {
		t.Errorf("output_config.effort = %v, want low", oc["effort"])
	}
}

// TestReasoningStyle_AdaptiveWithoutEffortOmitsOutputConfig: when the client sent only a
// budget there is no effort to forward. Deriving one would mean a second, inverse copy of
// the gateway's effort→budget table, and adaptive alone is a valid request.
func TestReasoningStyle_AdaptiveWithoutEffortOmitsOutputConfig(t *testing.T) {
	sent := captureThinking(t, ReasoningStyleAdaptive,
		&ports.ReasoningRequest{BudgetTokens: 8192})

	th, _ := sent["thinking"].(map[string]interface{})
	if th == nil || th["type"] != "adaptive" {
		t.Fatalf("thinking = %v, want type adaptive", sent["thinking"])
	}
	if _, present := sent["output_config"]; present {
		t.Errorf("output_config must be omitted when no effort was named, got %v", sent["output_config"])
	}
}

// TestReasoningStyle_DisabledSendsNothing: absence of the field is what "do not think" looks
// like on BOTH shapes. Inventing a disabled marker would send a field neither API documents.
func TestReasoningStyle_DisabledSendsNothing(t *testing.T) {
	for _, style := range []string{ReasoningStyleBudget, ReasoningStyleAdaptive} {
		sent := captureThinking(t, style,
			&ports.ReasoningRequest{Effort: ports.ReasoningEffortNone, Disabled: true, BudgetTokens: 1024})
		if _, present := sent["thinking"]; present {
			t.Errorf("style %q: thinking must be absent on an explicit disable", style)
		}
		if _, present := sent["output_config"]; present {
			t.Errorf("style %q: output_config must be absent on an explicit disable", style)
		}
	}
}

// TestReasoningStyle_NoRequestSendsNothing keeps a plain request byte-identical.
func TestReasoningStyle_NoRequestSendsNothing(t *testing.T) {
	for _, style := range []string{"", ReasoningStyleAdaptive} {
		sent := captureThinking(t, style, nil)
		if _, present := sent["thinking"]; present {
			t.Errorf("style %q: thinking must be absent when the client did not ask", style)
		}
		if _, present := sent["output_config"]; present {
			t.Errorf("style %q: output_config must be absent when the client did not ask", style)
		}
	}
}

// TestReasoningStyle_UnknownEffortIsNotForwarded: the effort reaches the wire only when it
// is one of the values the API accepts. A value we do not recognise is dropped rather than
// passed through, because output_config.effort is validated by the provider and an unknown
// string fails the whole request instead of the thinking part of it.
func TestReasoningStyle_UnknownEffortIsNotForwarded(t *testing.T) {
	sent := captureThinking(t, ReasoningStyleAdaptive,
		&ports.ReasoningRequest{Effort: "extreme", BudgetTokens: 1024})
	if _, present := sent["output_config"]; present {
		t.Errorf("an unrecognised effort must not be forwarded, got %v", sent["output_config"])
	}
	if th, _ := sent["thinking"].(map[string]interface{}); th == nil || th["type"] != "adaptive" {
		t.Errorf("thinking must still request adaptive, got %v", sent["thinking"])
	}
}
