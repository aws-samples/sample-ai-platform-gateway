// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package bedrock

import (
	"testing"

	btypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/aiplat/core/internal/ports"
)

// TestConvertToolsToBedrockConfig: tools in the boundary format become Bedrock's
// ToolConfiguration; an empty list returns nil (no ToolConfig).
func TestConvertToolsToBedrockConfig(t *testing.T) {
	if ConvertToolsToBedrockConfig(nil) != nil {
		t.Error("with no tools it should return nil")
	}
	tc := ConvertToolsToBedrockConfig([]ports.ToolDef{
		{Name: "get_weather", Description: "clima", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "", Description: "ignorada"}, // no name → discarded
	})
	if tc == nil {
		t.Fatal("with one valid tool it should return a ToolConfiguration")
	}
	if len(tc.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tc.Tools))
	}
}

// toolResultIDs extracts, in order, the toolUseIds of a Bedrock message that contains
// only toolResult blocks. It fails the test if any block is not a toolResult.
func toolResultIDs(t *testing.T, msg btypes.Message) []string {
	t.Helper()
	var ids []string
	for _, blk := range msg.Content {
		tr, ok := blk.(*btypes.ContentBlockMemberToolResult)
		if !ok {
			t.Fatalf("block is not a toolResult: %T", blk)
		}
		ids = append(ids, *tr.Value.ToolUseId)
	}
	return ids
}

// TestConvertMessagesParallelToolResults: two (or more) consecutive `tool` messages
// from the OpenAI dialect must become ONE single Bedrock `user` message with one
// toolResult block per result, in the original order. This is the
// "Expected toolResult blocks at messages.N.content for the following Ids" bug.
func TestConvertMessagesParallelToolResults(t *testing.T) {
	msgs := []ports.Message{
		{Role: "user", Text: "Compare weather in Sao Paulo and Rio."},
		{Role: "assistant", ToolCalls: []ports.ToolCall{
			{ID: "call_a", Name: "get_weather", Arguments: `{"city":"Sao Paulo"}`},
			{ID: "call_b", Name: "get_weather", Arguments: `{"city":"Rio"}`},
		}},
		{Role: "tool", ToolCallID: "call_a", Text: "Sao Paulo: 22C, cloudy"},
		{Role: "tool", ToolCallID: "call_b", Text: "Rio: 30C, sunny"},
	}

	_, conv := convertMessages(msgs)

	// user, assistant(2 toolUse), user(2 toolResult) → 3 messages, alternating.
	if len(conv) != 3 {
		t.Fatalf("expected 3 Bedrock messages, got %d", len(conv))
	}
	if conv[0].Role != btypes.ConversationRoleUser ||
		conv[1].Role != btypes.ConversationRoleAssistant ||
		conv[2].Role != btypes.ConversationRoleUser {
		t.Fatalf("role alternation broken: %v/%v/%v", conv[0].Role, conv[1].Role, conv[2].Role)
	}
	if n := len(conv[1].Content); n != 2 {
		t.Fatalf("assistant should have 2 toolUse blocks, got %d", n)
	}
	ids := toolResultIDs(t, conv[2])
	if len(ids) != 2 || ids[0] != "call_a" || ids[1] != "call_b" {
		t.Fatalf("wrong toolResult ids/order: %v (expected [call_a call_b])", ids)
	}
}

// TestConvertMessagesTripleParallelToolResults: same rule with 3 parallel tools —
// every toolResult in a single `user` turn, in order.
func TestConvertMessagesTripleParallelToolResults(t *testing.T) {
	msgs := []ports.Message{
		{Role: "user", Text: "weather for three cities"},
		{Role: "assistant", ToolCalls: []ports.ToolCall{
			{ID: "call_a", Name: "get_weather", Arguments: `{"city":"A"}`},
			{ID: "call_b", Name: "get_weather", Arguments: `{"city":"B"}`},
			{ID: "call_c", Name: "get_weather", Arguments: `{"city":"C"}`},
		}},
		{Role: "tool", ToolCallID: "call_a", Text: "A: 10C"},
		{Role: "tool", ToolCallID: "call_b", Text: "B: 20C"},
		{Role: "tool", ToolCallID: "call_c", Text: "C: 30C"},
	}

	_, conv := convertMessages(msgs)

	if len(conv) != 3 {
		t.Fatalf("expected 3 Bedrock messages, got %d", len(conv))
	}
	ids := toolResultIDs(t, conv[2])
	if len(ids) != 3 || ids[0] != "call_a" || ids[1] != "call_b" || ids[2] != "call_c" {
		t.Fatalf("wrong toolResult ids/order: %v (expected [call_a call_b call_c])", ids)
	}
}

// TestConvertMessagesSingleToolResult: do not regress the ONE tool call case — a
// `tool` message becomes a `user` message with a single toolResult, and a tool turn
// followed by another user question must not merge improperly.
func TestConvertMessagesSingleToolResult(t *testing.T) {
	msgs := []ports.Message{
		{Role: "user", Text: "weather in Rio?"},
		{Role: "assistant", ToolCalls: []ports.ToolCall{
			{ID: "call_a", Name: "get_weather", Arguments: `{"city":"Rio"}`},
		}},
		{Role: "tool", ToolCallID: "call_a", Text: "Rio: 30C"},
		{Role: "user", Text: "and Sao Paulo?"},
	}

	_, conv := convertMessages(msgs)

	if len(conv) != 4 {
		t.Fatalf("expected 4 Bedrock messages, got %d", len(conv))
	}
	ids := toolResultIDs(t, conv[2])
	if len(ids) != 1 || ids[0] != "call_a" {
		t.Fatalf("wrong toolResult ids/order: %v (expected [call_a])", ids)
	}
	if conv[3].Role != btypes.ConversationRoleUser {
		t.Fatalf("the user message after the tool result should stay separate")
	}
	if _, ok := conv[3].Content[0].(*btypes.ContentBlockMemberText); !ok {
		t.Fatalf("the following user message should be text, got %T", conv[3].Content[0])
	}
}

// TestCachePointType: the SDK's default cache point enum is available and is the
// string Bedrock expects ("default"). It guards against an SDK bump changing the
// prompt caching contract under our feet.
func TestCachePointType(t *testing.T) {
	if btypes.CachePointTypeDefault != "default" {
		t.Errorf("CachePointTypeDefault=%q, expected \"default\"", btypes.CachePointTypeDefault)
	}
	// The typed system cache point block must exist and be constructible.
	var _ btypes.SystemContentBlock = &btypes.SystemContentBlockMemberCachePoint{
		Value: btypes.CachePointBlock{Type: btypes.CachePointTypeDefault},
	}
}

// TestConvertMessagesIncludesImages is the regression for the bug where a
// multimodal request was routed to a capable model (hasImage + Capabilities.
// Multimodal in the routing eligibility filter) but convertMessages built every
// user message from Text alone — the image bytes never reached Bedrock, and the
// request looked correctly routed while silently serving text-only.
func TestConvertMessagesIncludesImages(t *testing.T) {
	msgs := []ports.Message{
		{Role: "user", Text: "o que é isto?", Images: []ports.ImagePart{
			{Format: "png", Bytes: []byte{1, 2, 3}},
		}},
	}
	_, conv := convertMessages(msgs)
	if len(conv) != 1 {
		t.Fatalf("expected 1 converted message, got %d", len(conv))
	}
	if len(conv[0].Content) != 2 {
		t.Fatalf("expected 2 content blocks (text + image), got %d", len(conv[0].Content))
	}
	if _, ok := conv[0].Content[0].(*btypes.ContentBlockMemberText); !ok {
		t.Fatalf("block 0 should be text, got %T", conv[0].Content[0])
	}
	img, ok := conv[0].Content[1].(*btypes.ContentBlockMemberImage)
	if !ok {
		t.Fatalf("block 1 should be an image, got %T", conv[0].Content[1])
	}
	if img.Value.Format != btypes.ImageFormatPng {
		t.Errorf("format = %v, want png", img.Value.Format)
	}
	src, ok := img.Value.Source.(*btypes.ImageSourceMemberBytes)
	if !ok {
		t.Fatalf("source should carry raw bytes, got %T", img.Value.Source)
	}
	if len(src.Value) != 3 {
		t.Errorf("image bytes = %v, want the original 3 bytes untouched", src.Value)
	}
}

// TestImageBlocksDropsUnknownFormat: an unsupported/unknown format is dropped
// silently rather than sent — Bedrock rejects an unknown ImageFormat for the WHOLE
// request, so one bad part must not take the rest of a valid message down with it.
func TestImageBlocksDropsUnknownFormat(t *testing.T) {
	blocks := imageBlocks([]ports.ImagePart{
		{Format: "bmp", Bytes: []byte{1}},     // unsupported by Bedrock's ImageFormat enum
		{Format: "png", Bytes: nil},           // empty bytes, nothing to send
		{Format: "JPEG", Bytes: []byte{1, 2}}, // format matching is case-insensitive
	})
	if len(blocks) != 1 {
		t.Fatalf("expected only the valid jpeg part to survive, got %d blocks", len(blocks))
	}
	img := blocks[0].(*btypes.ContentBlockMemberImage)
	if img.Value.Format != btypes.ImageFormatJpeg {
		t.Errorf("format = %v, want jpeg (case-insensitive match)", img.Value.Format)
	}
}
