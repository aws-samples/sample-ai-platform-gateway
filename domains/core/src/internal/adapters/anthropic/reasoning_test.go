// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aiplat/core/internal/ports"
)

// anthropicFrame used to return a bare string, so there was nowhere to put a thinking delta:
// a dialect could either drop the reasoning or blend it into the answer. The second is worse
// — every OpenAI SDK concatenates delta.content, so the model's private reasoning would print
// inside the answer an end user reads. This is the test that keeps the two apart.
func TestAnthropicFrame_ReasoningGoesToItsOwnField(t *testing.T) {
	var res ports.Result

	th := anthropicFrame(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"let me check"}}`, &res)
	if th.Reasoning != "let me check" {
		t.Errorf("Reasoning = %q, want the thinking text", th.Reasoning)
	}
	if th.Text != "" {
		t.Errorf("Text = %q; thinking must never be returned as answer content", th.Text)
	}

	sig := anthropicFrame(`{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"sig-1"}}`, &res)
	if sig.Signature != "sig-1" {
		t.Errorf("Signature = %q, want sig-1", sig.Signature)
	}
	if sig.Text != "" || sig.Reasoning != "" {
		t.Error("a signature is opaque metadata, never content")
	}

	txt := anthropicFrame(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"the answer"}}`, &res)
	if txt.Text != "the answer" || txt.Reasoning != "" {
		t.Errorf("answer delta = %+v, want only Text", txt)
	}
}

// Redacted thinking arrives as a whole content BLOCK, not as a delta — the encrypted payload
// is on the start event. Recording it is what keeps "the model did not think" distinguishable
// from "we were not allowed to see it".
func TestAnthropicFrame_RedactedThinkingIsMarked(t *testing.T) {
	var res ports.Result
	c := anthropicFrame(`{"type":"content_block_start","content_block":{"type":"redacted_thinking","data":"encrypted"}}`, &res)
	if !c.Redacted {
		t.Error("a redacted_thinking block must be reported as redacted")
	}
	if c.Reasoning != "" {
		t.Errorf("encrypted content must not be presented as readable reasoning: %q", c.Reasoning)
	}
}

// Usage accounting is unchanged by the wider return type. It is split across two events and
// both halves are needed: message_start's output_tokens is a placeholder, so taking it would
// give every response a cost of one token.
func TestAnthropicFrame_UsageStillAccumulates(t *testing.T) {
	var res ports.Result
	anthropicFrame(`{"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":1,"cache_read_input_tokens":10}}}`, &res)
	if res.InputTokens != 50 {
		t.Errorf("InputTokens = %d, want 50", res.InputTokens)
	}
	if res.OutputTokens != 0 {
		t.Errorf("OutputTokens = %d; message_start's value is a placeholder and must be ignored", res.OutputTokens)
	}
	if res.CacheReadInputTokens != 10 || res.CacheCounters != ports.CacheCountersReported {
		t.Errorf("cache counters lost: read=%d conv=%q", res.CacheReadInputTokens, res.CacheCounters)
	}
	anthropicFrame(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":200}}`, &res)
	if res.OutputTokens != 200 || res.StopReason != "end_turn" {
		t.Errorf("final usage/stop lost: out=%d stop=%q", res.OutputTokens, res.StopReason)
	}
}

// ── the BUFFERED path, which had the sharper version of the same bug ──────────
//
// Invoke read `d.Content[0].Text`. That is correct for a plain answer, which has exactly one
// text block — so it looked fine and stayed fine for as long as thinking could not be
// requested. With thinking enabled the response is `[{type:"thinking"},{type:"text"}]` and a
// thinking block carries NO `text` field, so the old code returned an empty answer. Not
// "reasoning dropped": no answer at all, on every reasoning request.
func bufferedInvoke(t *testing.T, respJSON string) ports.Result {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, respJSON)
	}))
	t.Cleanup(srv.Close)
	a := &Adapter{HTTP: srv.Client(), BaseURL: srv.URL, ModelID: "claude-x", APIKey: "k"}
	res, err := a.Invoke(context.Background(), ports.InvokeInput{
		Messages: []ports.Message{{Role: "user", Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	return res
}

func TestInvoke_ThinkingBlockDoesNotSwallowTheAnswer(t *testing.T) {
	res := bufferedInvoke(t, `{"content":[
		{"type":"thinking","thinking":"let me reason","signature":"sig-7"},
		{"type":"text","text":"The answer is 8."}
	],"usage":{"input_tokens":10,"output_tokens":40}}`)

	if res.Text != "The answer is 8." {
		t.Errorf("Text = %q, want the answer — a thinking block must not consume it", res.Text)
	}
	if res.Reasoning != "let me reason" {
		t.Errorf("Reasoning = %q", res.Reasoning)
	}
	if res.ReasoningSignature != "sig-7" {
		t.Errorf("ReasoningSignature = %q, want it kept for the next turn", res.ReasoningSignature)
	}
	if res.ReasoningChars != len("let me reason") {
		t.Errorf("ReasoningChars = %d", res.ReasoningChars)
	}
}

// A multi-block answer used to lose everything after the first block.
func TestInvoke_ConcatenatesEveryTextBlock(t *testing.T) {
	res := bufferedInvoke(t, `{"content":[{"type":"text","text":"part one "},{"type":"text","text":"part two"}],
		"usage":{"input_tokens":1,"output_tokens":2}}`)
	if res.Text != "part one part two" {
		t.Errorf("Text = %q, want both blocks", res.Text)
	}
}

func TestInvoke_RedactedThinkingIsMarked(t *testing.T) {
	res := bufferedInvoke(t, `{"content":[{"type":"redacted_thinking","data":"encrypted"},{"type":"text","text":"done"}],
		"usage":{"input_tokens":1,"output_tokens":2}}`)
	if !res.ReasoningRedacted {
		t.Error("encrypted thinking must stay distinguishable from not thinking")
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want the answer", res.Text)
	}
	if res.Reasoning != "" {
		t.Errorf("redacted content must not be presented as readable reasoning: %q", res.Reasoning)
	}
}

// A response with no thinking must behave exactly as before: the fix is additive, and a
// non-thinking answer is the overwhelmingly common case.
func TestInvoke_PlainAnswerUnchanged(t *testing.T) {
	res := bufferedInvoke(t, `{"content":[{"type":"text","text":"plain"}],
		"usage":{"input_tokens":5,"output_tokens":7,"cache_read_input_tokens":3}}`)
	if res.Text != "plain" || res.InputTokens != 5 || res.OutputTokens != 7 {
		t.Errorf("plain answer changed: %+v", res)
	}
	if res.Reasoning != "" || res.ReasoningChars != 0 || res.ReasoningRedacted {
		t.Errorf("no reasoning must mean no reasoning fields: %+v", res)
	}
	if res.CacheReadInputTokens != 3 || res.CacheCounters != ports.CacheCountersReported {
		t.Errorf("cache counters lost: %+v", res)
	}
}
