// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package google

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aiplat/core/internal/ports"
)

// Gemini marks reasoning with `thought: true` on an ORDINARY text part — not a separate part
// type. So a reader that ignores the flag appends the model's thinking straight into the
// answer, which is worse than dropping it. This test is the reason that flag is read.
func TestGeminiFrame_ThoughtPartsAreSeparatedFromTheAnswer(t *testing.T) {
	var res ports.Result
	c := geminiFrame(`{"candidates":[{"content":{"parts":[
		{"text":"first I convert the units","thought":true},
		{"text":"It is 42 km."}
	]}}]}`, &res)
	if c.Reasoning != "first I convert the units" {
		t.Errorf("Reasoning = %q, want the thought part", c.Reasoning)
	}
	if c.Text != "It is 42 km." {
		t.Errorf("Text = %q, want only the non-thought part", c.Text)
	}
}

func TestGeminiFrame_ThoughtSignatureIsKept(t *testing.T) {
	var res ports.Result
	c := geminiFrame(`{"candidates":[{"content":{"parts":[{"text":"x","thought":true,"thoughtSignature":"sig-9"}]}}]}`, &res)
	if c.Signature != "sig-9" {
		t.Errorf("Signature = %q, want sig-9 — it is what makes the chain continuable", c.Signature)
	}
}

// usageMetadata is CUMULATIVE, so the counters are assigned and never added. thoughtsTokenCount
// is reported separately but is ALREADY inside candidatesTokenCount, so adding it would
// double-charge the thinking.
func TestGeminiFrame_ThinkingTokensAreNotDoubleCounted(t *testing.T) {
	var res ports.Result
	geminiFrame(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":100,"thoughtsTokenCount":60}}`, &res)
	if res.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", res.InputTokens)
	}
	if res.OutputTokens != 100 {
		t.Errorf("OutputTokens = %d, want 100 — thoughts are already included, never added", res.OutputTokens)
	}
}

// A response with no thought parts must behave exactly as before: multi-part chunks
// concatenated in order, nothing routed to reasoning.
func TestGeminiFrame_NoThoughtPartsBehavesAsBefore(t *testing.T) {
	var res ports.Result
	c := geminiFrame(`{"candidates":[{"content":{"parts":[{"text":"a"},{"text":"b"}]},"finishReason":"STOP"}]}`, &res)
	if c.Text != "ab" {
		t.Errorf("Text = %q, want the parts concatenated in order", c.Text)
	}
	if c.Reasoning != "" {
		t.Errorf("Reasoning = %q, want empty", c.Reasoning)
	}
	if res.StopReason != "STOP" {
		t.Errorf("StopReason = %q, want STOP", res.StopReason)
	}
}

// generationConfig must be OMITTED entirely when the client asked for nothing: an empty object
// would be accepted, but it invites the next reader to start filling it with defaults.
func TestGenerationConfig_EmptyWhenNothingRequested(t *testing.T) {
	if gc := generationConfig(ports.InvokeInput{}); len(gc) != 0 {
		t.Errorf("generationConfig = %v, want empty", gc)
	}
}

// ── the BUFFERED path ─────────────────────────────────────────────────────────
//
// Invoke read `Parts[0].Text`. Two bugs in one line: any multi-part answer was truncated to its
// first part (the streaming path's own comment already said so), and once thinkingConfig is
// sent with includeThoughts, part 0 IS the thinking — so the client would have received the
// model's reasoning presented as the answer, with the real answer discarded.
func bufferedInvoke(t *testing.T, respJSON string) ports.Result {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, respJSON)
	}))
	t.Cleanup(srv.Close)
	a := &Adapter{HTTP: srv.Client(), BaseURL: srv.URL, ModelID: "gemini-x", APIKey: "k"}
	res, err := a.Invoke(context.Background(), ports.InvokeInput{
		Messages: []ports.Message{{Role: "user", Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	return res
}

func TestInvoke_ThoughtPartIsNotReturnedAsTheAnswer(t *testing.T) {
	res := bufferedInvoke(t, `{"candidates":[{"content":{"parts":[
		{"text":"first I convert the units","thought":true,"thoughtSignature":"sig-3"},
		{"text":"It is 42 km."}
	]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":100,"thoughtsTokenCount":60}}`)

	if res.Text != "It is 42 km." {
		t.Errorf("Text = %q, want the answer — the thought part must not be returned as it", res.Text)
	}
	if res.Reasoning != "first I convert the units" {
		t.Errorf("Reasoning = %q", res.Reasoning)
	}
	if res.ReasoningSignature != "sig-3" {
		t.Errorf("ReasoningSignature = %q, want sig-3", res.ReasoningSignature)
	}
	// thoughtsTokenCount is already inside candidatesTokenCount; adding it would double-charge.
	if res.OutputTokens != 100 {
		t.Errorf("OutputTokens = %d, want 100", res.OutputTokens)
	}
}

func TestInvoke_ConcatenatesEveryPart(t *testing.T) {
	res := bufferedInvoke(t, `{"candidates":[{"content":{"parts":[{"text":"a"},{"text":"b"},{"text":"c"}]}}],
		"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":3}}`)
	if res.Text != "abc" {
		t.Errorf("Text = %q, want every part concatenated in order", res.Text)
	}
}

func TestInvoke_PlainAnswerUnchanged(t *testing.T) {
	res := bufferedInvoke(t, `{"candidates":[{"content":{"parts":[{"text":"plain"}]}}],
		"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":7,"cachedContentTokenCount":2}}`)
	if res.Text != "plain" || res.InputTokens != 5 || res.OutputTokens != 7 {
		t.Errorf("plain answer changed: %+v", res)
	}
	if res.Reasoning != "" || res.ReasoningChars != 0 {
		t.Errorf("no reasoning must mean no reasoning fields: %+v", res)
	}
	if res.CacheReadInputTokens != 2 || res.CacheCounters != ports.CacheCountersReported {
		t.Errorf("cache counters lost: %+v", res)
	}
}
