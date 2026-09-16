// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package bedrock

import (
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	btypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/aiplat/core/internal/ports"
)

// reasoningDelta builds the event an extended-thinking model emits DURING the think phase —
// the one this adapter used to throw away.
func reasoningDelta(s string) btypes.ConverseStreamOutput {
	return &btypes.ConverseStreamOutputMemberContentBlockDelta{Value: btypes.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(0),
		Delta: &btypes.ContentBlockDeltaMemberReasoningContent{
			Value: &btypes.ReasoningContentBlockDeltaMemberText{Value: s},
		},
	}}
}

func signatureDelta(s string) btypes.ConverseStreamOutput {
	return &btypes.ConverseStreamOutputMemberContentBlockDelta{Value: btypes.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(0),
		Delta: &btypes.ContentBlockDeltaMemberReasoningContent{
			Value: &btypes.ReasoningContentBlockDeltaMemberSignature{Value: s},
		},
	}}
}

func redactedDelta() btypes.ConverseStreamOutput {
	return &btypes.ConverseStreamOutputMemberContentBlockDelta{Value: btypes.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(0),
		Delta: &btypes.ContentBlockDeltaMemberReasoningContent{
			Value: &btypes.ReasoningContentBlockDeltaMemberRedactedContent{Value: []byte("opaque")},
		},
	}}
}

// ── the defect this fixes ─────────────────────────────────────────────────────
//
// The delta type switch handled Text and ToolUse with no default, so reasoningContent was
// discarded silently. A reasoning model streams its chain of thought while it thinks, which
// means bytes WERE arriving the whole time and this adapter dropped them — so "the model goes
// quiet for minutes" was produced here, not by the provider or the transport.
//
// Against the previous code this test fails on the first assertion: RecvChunk did not exist,
// and Recv skipped straight to the answer, so the reasoning was unobservable.
func TestRecvChunk_ForwardsReasoningSeparatelyFromAnswer(t *testing.T) {
	s := newTestStream(&fakeES{},
		reasoningDelta("let me think: "),
		reasoningDelta("2+2 is 4"),
		signatureDelta("sig-abc"),
		textDelta("The answer is 4."),
	)

	var reasoning, text, sig string
	for {
		c, err := s.RecvChunk()
		reasoning += c.Reasoning
		text += c.Text
		if c.Signature != "" {
			sig = c.Signature
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("unexpected error: %v", err)
			}
			break
		}
	}

	if reasoning != "let me think: 2+2 is 4" {
		t.Errorf("reasoning = %q, want the full chain of thought", reasoning)
	}
	// The separation is the correctness point: every OpenAI SDK concatenates
	// delta.content, so thinking leaking into Text would print the model's reasoning
	// inside the answer.
	if text != "The answer is 4." {
		t.Errorf("text = %q, reasoning must NOT be mixed into the answer", text)
	}
	if sig != "sig-abc" {
		t.Errorf("signature = %q, want it carried through", sig)
	}

	res := s.Result()
	if res.Reasoning != "let me think: 2+2 is 4" {
		t.Errorf("Result.Reasoning = %q", res.Reasoning)
	}
	if res.Text != "The answer is 4." {
		t.Errorf("Result.Text = %q", res.Text)
	}
	// The signature is what allows a reasoning chain to continue across turns. Dropping it
	// would leave the thinking text stored and the chain unusable.
	if res.ReasoningSignature != "sig-abc" {
		t.Errorf("Result.ReasoningSignature = %q", res.ReasoningSignature)
	}
	if res.ReasoningChars != len("let me think: 2+2 is 4") {
		t.Errorf("ReasoningChars = %d", res.ReasoningChars)
	}
}

// Recv is the narrow read used by callers that only want the answer. It must SKIP reasoning
// rather than return it, otherwise every existing caller starts printing chain of thought.
func TestRecv_SkipsReasoningAndReturnsOnlyAnswerText(t *testing.T) {
	s := newTestStream(&fakeES{},
		reasoningDelta("thinking hard"),
		textDelta("hello "),
		reasoningDelta("more thinking"),
		textDelta("world"),
	)
	var got string
	for {
		txt, err := s.Recv()
		got += txt
		if err != nil {
			break
		}
	}
	if got != "hello world" {
		t.Errorf("Recv gave %q, want only the answer text", got)
	}
	if s.Result().Reasoning != "thinking hardmore thinking" {
		t.Errorf("reasoning must still be accumulated: %q", s.Result().Reasoning)
	}
}

// Encrypted thinking has to stay distinguishable from "the model did not think".
func TestRecvChunk_RedactedReasoningIsRecorded(t *testing.T) {
	s := newTestStream(&fakeES{}, redactedDelta(), textDelta("ok"))
	sawRedacted := false
	for {
		c, err := s.RecvChunk()
		if c.Redacted {
			sawRedacted = true
		}
		if err != nil {
			break
		}
	}
	if !sawRedacted {
		t.Error("the redacted marker must reach the caller")
	}
	if !s.Result().ReasoningRedacted {
		t.Error("Result must record that thinking happened but was not readable")
	}
	if s.Result().Reasoning != "" {
		t.Error("redacted content must not be presented as readable reasoning")
	}
}

// The missing default is the bug GENERATOR, not just the reasoning case: the SDK defines six
// delta members and this switch acts on four, so anything new would vanish the same way.
// An unknown member must be consumed without breaking the stream, and must be visible.
func TestRecvChunk_UnknownDeltaDoesNotBreakTheStream(t *testing.T) {
	unknown := &btypes.ConverseStreamOutputMemberContentBlockDelta{Value: btypes.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(0),
		// Citation is a real SDK member this adapter does not act on.
		Delta: &btypes.ContentBlockDeltaMemberCitation{},
	}}
	s := newTestStream(&fakeES{}, unknown, textDelta("still here"))
	var got string
	for {
		c, err := s.RecvChunk()
		got += c.Text
		if err != nil {
			break
		}
	}
	if got != "still here" {
		t.Errorf("an unrecognised delta must be skipped, not fatal: got %q", got)
	}
	if !s.warnedUnknown {
		t.Error("an unrecognised delta must leave a trace; silence is how reasoningContent was lost")
	}
}

// The adapter must satisfy the optional interface, otherwise the pump silently falls back to
// the answer-only read and the reasoning never reaches the client.
func TestBrStreamImplementsReasoningStream(t *testing.T) {
	var s ports.ProviderStream = &brStream{}
	if _, ok := s.(ports.ReasoningStream); !ok {
		t.Fatal("brStream must implement ports.ReasoningStream")
	}
}

// ── the same defect, in the buffered path ─────────────────────────────────────
//
// callBedrock's content-block switch handled Text and ToolUse only. A thinking model
// returns its chain of thought as its OWN block, so the non-streaming path dropped both
// the reasoning and the signature. This one hid better than the streaming version: the
// answer still arrived, so nothing looked broken — while the signature that lets the
// chain continue on a later turn was discarded on every request.
func converseOut(blocks ...btypes.ContentBlock) *bedrockruntime.ConverseOutput {
	return &bedrockruntime.ConverseOutput{
		Output:     &btypes.ConverseOutputMemberMessage{Value: btypes.Message{Content: blocks}},
		StopReason: btypes.StopReasonEndTurn,
		Usage: &btypes.TokenUsage{
			InputTokens: aws.Int32(10), OutputTokens: aws.Int32(40), TotalTokens: aws.Int32(50),
		},
	}
}

func TestParseConverseOutput_KeepsReasoningAndSignature(t *testing.T) {
	out := converseOut(
		&btypes.ContentBlockMemberReasoningContent{
			Value: &btypes.ReasoningContentBlockMemberReasoningText{
				Value: btypes.ReasoningTextBlock{
					Text:      aws.String("first I check the units"),
					Signature: aws.String("sig-xyz"),
				},
			},
		},
		&btypes.ContentBlockMemberText{Value: "It is 42 km."},
	)

	res, err := parseConverseOutput(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Reasoning != "first I check the units" {
		t.Errorf("Reasoning = %q, want the chain of thought", res.Reasoning)
	}
	// Same separation rule as streaming: reasoning must never end up inside the answer.
	if res.Text != "It is 42 km." {
		t.Errorf("Text = %q, reasoning must not be mixed into the answer", res.Text)
	}
	if res.ReasoningSignature != "sig-xyz" {
		t.Errorf("ReasoningSignature = %q, want it kept for the next turn", res.ReasoningSignature)
	}
	if res.ReasoningChars != len("first I check the units") {
		t.Errorf("ReasoningChars = %d", res.ReasoningChars)
	}
	// Reasoning tokens are already inside OutputTokens; the counters must not change.
	if res.OutputTokens != 40 || res.InputTokens != 10 {
		t.Errorf("tokens = %d/%d, want 10/40 unchanged", res.InputTokens, res.OutputTokens)
	}
}

func TestParseConverseOutput_RedactedReasoningIsRecorded(t *testing.T) {
	out := converseOut(
		&btypes.ContentBlockMemberReasoningContent{
			Value: &btypes.ReasoningContentBlockMemberRedactedContent{Value: []byte("opaque")},
		},
		&btypes.ContentBlockMemberText{Value: "done"},
	)
	res, err := parseConverseOutput(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ReasoningRedacted {
		t.Error("encrypted thinking must stay distinguishable from not thinking")
	}
	if res.Reasoning != "" {
		t.Errorf("redacted content must not be presented as readable reasoning: %q", res.Reasoning)
	}
}

// A response with no reasoning must stay exactly as it was — the fix is additive, and a
// non-thinking model is the overwhelmingly common case.
func TestParseConverseOutput_NoReasoningLeavesFieldsEmpty(t *testing.T) {
	res, err := parseConverseOutput(converseOut(&btypes.ContentBlockMemberText{Value: "plain"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "plain" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Reasoning != "" || res.ReasoningChars != 0 || res.ReasoningSignature != "" || res.ReasoningRedacted {
		t.Errorf("no reasoning must mean no reasoning fields: %+v", res)
	}
}
