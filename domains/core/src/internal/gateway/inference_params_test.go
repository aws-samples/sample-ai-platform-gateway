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

	"github.com/aiplat/core/internal/adapters/anthropic"
	"github.com/aiplat/core/internal/ports"
)

// ── the defect these tests exist for ──────────────────────────────────────────
//
// max_tokens and temperature were PARSED from the request, used for the cache key and for
// the routing decision, and then dropped before the provider call. `ports.InvokeInput` even
// had a MaxOutputTokens field — no adapter read it. Measured against the live deployment: a
// request sending max_tokens 400 came back with 560 completion tokens, because the ceiling
// never left this process.
//
// What makes it a silent defect rather than an obvious one is that the request SUCCEEDS. The
// customer gets an answer, pays for tokens they tried to cap, and nothing anywhere reports
// that their parameter was ignored.

// capture runs one buffered provider call against a local server and returns the body the
// adapter actually put on the wire.
func capture(t *testing.T, r Route, canned string, inf invocation) map[string]interface{} {
	t.Helper()
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ = io.ReadAll(req.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, canned)
	}))
	t.Cleanup(srv.Close)
	r.BaseURL = srv.URL

	msgs := []chatMsg{{Role: "user", Content: json.RawMessage(`"hi"`)}}
	if _, err := callProvider(context.Background(), r, msgs, nil, inf); err != nil {
		t.Fatalf("callProvider(%s): %v", r.Provider, err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, string(raw))
	}
	return body
}

const (
	cannedOpenAI    = `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	cannedAnthropic = `{"content":[{"text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	cannedGemini    = `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`
)

func fp(v float64) *float64 { return &v }

// fullParams is a request that sets every sampling parameter, so a dropped one shows up as a
// missing key rather than as a plausible-looking value.
func fullParams() invocation {
	return invocation{
		MaxOutputTokens: 321,
		Params: ports.InferenceParams{
			Temperature: fp(0.2),
			TopP:        fp(0.9),
			Stop:        []string{"END"},
		},
	}
}

func TestInferenceParams_ReachOpenAICompatWire(t *testing.T) {
	body := capture(t, Route{Provider: "openai_compatible", ProviderModelID: "gpt-x"}, cannedOpenAI, fullParams())
	for k, want := range map[string]interface{}{
		"max_tokens": float64(321), "temperature": 0.2, "top_p": 0.9,
	} {
		if body[k] != want {
			t.Errorf("%s = %v (%T), want %v — the client's parameter never reached the provider", k, body[k], body[k], want)
		}
	}
	stop, _ := body["stop"].([]interface{})
	if len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop = %v, want [END]", body["stop"])
	}
}

func TestInferenceParams_ReachAnthropicWire(t *testing.T) {
	body := capture(t, Route{Provider: "anthropic", ProviderModelID: "claude-x"}, cannedAnthropic, fullParams())
	// The regression that mattered most on this adapter: max_tokens was HARDCODED to 1024,
	// so every answer was truncated there and no client could change it.
	if body["max_tokens"] != float64(321) {
		t.Errorf("max_tokens = %v, want 321 (it used to be hardcoded to 1024)", body["max_tokens"])
	}
	if body["temperature"] != 0.2 || body["top_p"] != 0.9 {
		t.Errorf("temperature/top_p = %v/%v, want 0.2/0.9", body["temperature"], body["top_p"])
	}
	// Anthropic's name for it is stop_sequences, not stop. Sending the wrong key is silently
	// ignored by the API, which looks exactly like never sending it.
	seqs, _ := body["stop_sequences"].([]interface{})
	if len(seqs) != 1 || seqs[0] != "END" {
		t.Errorf("stop_sequences = %v, want [END]", body["stop_sequences"])
	}
}

func TestInferenceParams_ReachGeminiWire(t *testing.T) {
	body := capture(t, Route{Provider: "google", ProviderModelID: "gemini-x"}, cannedGemini, fullParams())
	gc, ok := body["generationConfig"].(map[string]interface{})
	if !ok {
		t.Fatalf("no generationConfig on the wire: %v", body)
	}
	// camelCase, and Gemini IGNORES a key it does not recognise — so a snake_case slip here
	// would not fail, it would quietly stop honouring the parameter.
	if gc["maxOutputTokens"] != float64(321) || gc["temperature"] != 0.2 || gc["topP"] != 0.9 {
		t.Errorf("generationConfig = %v, want maxOutputTokens/temperature/topP set", gc)
	}
	seqs, _ := gc["stopSequences"].([]interface{})
	if len(seqs) != 1 || seqs[0] != "END" {
		t.Errorf("stopSequences = %v, want [END]", gc["stopSequences"])
	}
}

// A request that names NO parameter must put the same bytes on the wire as before this
// feature existed. Every field is conditional for exactly this reason: sending a zero we
// invented is not neutral — temperature 0 makes the model deterministic.
func TestInferenceParams_AbsentParamsAreNotSent(t *testing.T) {
	cases := []struct {
		route  Route
		canned string
		// forbidden keys at the top level of the body
		keys []string
	}{
		{Route{Provider: "openai_compatible", ProviderModelID: "m"}, cannedOpenAI,
			[]string{"max_tokens", "temperature", "top_p", "stop", "reasoning_effort"}},
		{Route{Provider: "google", ProviderModelID: "m"}, cannedGemini,
			[]string{"generationConfig"}},
	}
	for _, tc := range cases {
		body := capture(t, tc.route, tc.canned, invocation{})
		for _, k := range tc.keys {
			if _, present := body[k]; present {
				t.Errorf("%s: %q must be ABSENT when the client sent nothing, got %v",
					tc.route.Provider, k, body[k])
			}
		}
	}
	// Anthropic is the exception and has to be: the Messages API REQUIRES max_tokens, so
	// something must be sent. It is the documented default, not a client value.
	body := capture(t, Route{Provider: "anthropic", ProviderModelID: "m"}, cannedAnthropic, invocation{})
	if body["max_tokens"] != float64(anthropic.DefaultMaxTokens) {
		t.Errorf("anthropic max_tokens = %v, want the default %d", body["max_tokens"], anthropic.DefaultMaxTokens)
	}
	for _, k := range []string{"temperature", "top_p", "stop_sequences", "thinking"} {
		if _, present := body[k]; present {
			t.Errorf("anthropic: %q must be absent when unset, got %v", k, body[k])
		}
	}
}

// ── the reasoning opt-in on the wire ──────────────────────────────────────────

func thinkingParams(effort string, budget int) invocation {
	return invocation{
		MaxOutputTokens: 4096,
		Params: ports.InferenceParams{
			Reasoning: &ports.ReasoningRequest{Effort: effort, BudgetTokens: budget},
		},
	}
}

func TestReasoning_AnthropicGetsAThinkingBlock(t *testing.T) {
	body := capture(t, Route{Provider: "anthropic", ProviderModelID: "claude-x"}, cannedAnthropic,
		thinkingParams(ports.ReasoningEffortMedium, 2048))
	th, ok := body["thinking"].(map[string]interface{})
	if !ok {
		t.Fatalf("no thinking block on the wire: %v", body)
	}
	if th["type"] != "enabled" || th["budget_tokens"] != float64(2048) {
		t.Errorf("thinking = %v, want enabled with budget_tokens 2048", th)
	}
	// The API's own invariant. Violating it is a 400 from the provider, so the budget and
	// the ceiling have to be reconciled before they get here.
	if body["max_tokens"].(float64) <= th["budget_tokens"].(float64) {
		t.Errorf("max_tokens (%v) must exceed budget_tokens (%v)", body["max_tokens"], th["budget_tokens"])
	}
}

// This dialect takes the WORD, not a token count. Translating the effort into a number here
// would be inventing a mapping the provider already has an opinion about.
func TestReasoning_OpenAICompatGetsTheEffortWord(t *testing.T) {
	body := capture(t, Route{Provider: "openai_compatible", ProviderModelID: "m"}, cannedOpenAI,
		thinkingParams(ports.ReasoningEffortHigh, 16384))
	if body["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v, want \"high\"", body["reasoning_effort"])
	}
	if _, present := body["thinking"]; present {
		t.Error("the Anthropic thinking block must not leak into the OpenAI dialect")
	}
}

func TestReasoning_GeminiGetsThinkingConfigWithVisibleThoughts(t *testing.T) {
	body := capture(t, Route{Provider: "google", ProviderModelID: "m"}, cannedGemini,
		thinkingParams(ports.ReasoningEffortLow, 1024))
	gc := body["generationConfig"].(map[string]interface{})
	tc, ok := gc["thinkingConfig"].(map[string]interface{})
	if !ok {
		t.Fatalf("no thinkingConfig: %v", gc)
	}
	if tc["thinkingBudget"] != float64(1024) {
		t.Errorf("thinkingBudget = %v, want 1024", tc["thinkingBudget"])
	}
	// Without includeThoughts, Gemini thinks, bills the thinking as output tokens, and
	// returns nothing to show for it — the customer pays for reasoning they cannot read.
	if tc["includeThoughts"] != true {
		t.Error("includeThoughts must be true, or the reasoning is billed and invisible")
	}
}

// An explicit "do not think" is not the same as saying nothing, and Gemini is the provider
// that can actually honour it.
func TestReasoning_DisabledSendsAZeroBudgetToGemini(t *testing.T) {
	body := capture(t, Route{Provider: "google", ProviderModelID: "m"}, cannedGemini, invocation{
		Params: ports.InferenceParams{
			Reasoning: &ports.ReasoningRequest{Effort: ports.ReasoningEffortNone, Disabled: true},
		},
	})
	tc := body["generationConfig"].(map[string]interface{})["thinkingConfig"].(map[string]interface{})
	if tc["thinkingBudget"] != float64(0) || tc["includeThoughts"] != false {
		t.Errorf("thinkingConfig = %v, want a zero budget with includeThoughts false", tc)
	}
}
