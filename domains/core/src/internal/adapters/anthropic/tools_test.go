// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/aiplat/core/internal/ports"
)

// ── the defect ────────────────────────────────────────────────────────────────
//
// This adapter sent the OPENAI dialect to Anthropic's Messages API: `tool_calls`,
// `tool_call_id`, `name`, and `content` forwarded verbatim from the client — none of which
// exist in this API. Anthropic ignores fields it does not recognise, so nothing failed. A
// request carrying tools got a prose answer and no tool call; a tool result was invisible, so a
// tool conversation could not progress past the first call; and a multimodal message arrived as
// an OpenAI `image_url` part the model cannot read. `in.Tools` was never read at all.

func marshal(t *testing.T, v interface{}) map[string]interface{} {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestToWireTools_UsesInputSchemaNotTheOpenAIEnvelope(t *testing.T) {
	tools := toWireTools([]ports.ToolDef{{
		Name:        "get_weather",
		Description: "current weather",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{"city": map[string]interface{}{"type": "string"}}},
	}})
	if len(tools) != 1 {
		t.Fatalf("got %d tools", len(tools))
	}
	m := marshal(t, tools[0])
	// `input_schema`, not the OpenAI dialect's nested function.parameters.
	if _, ok := m["input_schema"]; !ok {
		t.Errorf("no input_schema on the declaration: %v", m)
	}
	if m["name"] != "get_weather" || m["description"] != "current weather" {
		t.Errorf("declaration = %v", m)
	}
	if _, leaked := m["function"]; leaked {
		t.Error("the OpenAI `function` envelope must not be sent to Anthropic")
	}
}

// A tool with no parameters still needs a schema: Anthropic rejects a missing input_schema,
// and an empty object is how "no arguments" is expressed.
func TestToWireTools_NoParametersStillSendsASchema(t *testing.T) {
	tools := toWireTools([]ports.ToolDef{{Name: "ping"}})
	m := marshal(t, tools[0])
	sch, ok := m["input_schema"].(map[string]interface{})
	if !ok {
		t.Fatalf("input_schema missing or wrong type: %v", m)
	}
	if sch["type"] != "object" {
		t.Errorf("input_schema = %v, want an empty object schema", sch)
	}
	// A nameless tool cannot be declared and must be dropped rather than sent as `""`.
	if got := toWireTools([]ports.ToolDef{{Name: ""}}); got != nil {
		t.Errorf("a nameless tool must be dropped, got %v", got)
	}
}

// An assistant turn that requested tools must go back as tool_use BLOCKS, with the arguments
// parsed into an object — Anthropic takes `input` as a struct, not the dialect's JSON string.
func TestToWireMessages_AssistantToolCallsBecomeToolUseBlocks(t *testing.T) {
	msgs := []ports.Message{{
		Role: "assistant", Text: "let me look that up",
		ToolCalls: []ports.ToolCall{{ID: "toolu_1", Name: "get_weather", Arguments: `{"city":"SP"}`}},
	}}
	w := toWireMessages(msgs)
	if len(w) != 1 || w[0].Role != "assistant" || len(w[0].Content) != 2 {
		t.Fatalf("got %+v", w)
	}
	if w[0].Content[0].Type != "text" || w[0].Content[0].Text != "let me look that up" {
		t.Errorf("block 0 = %+v, want the text", w[0].Content[0])
	}
	tu := w[0].Content[1]
	if tu.Type != "tool_use" || tu.ID != "toolu_1" || tu.Name != "get_weather" {
		t.Errorf("block 1 = %+v, want the tool_use", tu)
	}
	if tu.Input["city"] != "SP" {
		t.Errorf("input = %v, want the arguments parsed into an object", tu.Input)
	}
}

// The rule that gets a conversation REJECTED when it is wrong: in the OpenAI dialect each tool
// result is its own message, while Anthropic requires all the tool_result blocks of a turn
// grouped inside ONE user message.
func TestToWireMessages_ConsecutiveToolResultsMergeIntoOneUserTurn(t *testing.T) {
	msgs := []ports.Message{
		{Role: "assistant", ToolCalls: []ports.ToolCall{
			{ID: "a", Name: "t1", Arguments: "{}"}, {ID: "b", Name: "t2", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "a", Text: "result A"},
		{Role: "tool", ToolCallID: "b", Text: "result B"},
		{Role: "user", Text: "and now?"},
	}
	w := toWireMessages(msgs)
	if len(w) != 3 {
		t.Fatalf("want assistant + ONE merged user + user, got %d: %+v", len(w), w)
	}
	if w[1].Role != "user" || len(w[1].Content) != 2 {
		t.Fatalf("the two results must be one user turn with two blocks: %+v", w[1])
	}
	for i, want := range []struct{ id, text string }{{"a", "result A"}, {"b", "result B"}} {
		b := w[1].Content[i]
		if b.Type != "tool_result" || b.ToolUseID != want.id || b.ResultContent != want.text {
			t.Errorf("block %d = %+v, want tool_result %s", i, b, want.id)
		}
	}
	// The message AFTER the merged run must not be swallowed by the merge loop.
	if w[2].Role != "user" || w[2].Content[0].Text != "and now?" {
		t.Errorf("the message after the tool run was lost: %+v", w[2])
	}
}

// A tool result with no id cannot be attributed and is dropped rather than sent with an empty
// tool_use_id, which Anthropic rejects.
func TestToWireMessages_ToolResultWithoutIDIsDropped(t *testing.T) {
	w := toWireMessages([]ports.Message{{Role: "tool", Text: "orphan"}})
	if len(w) != 0 {
		t.Errorf("an unattributable tool result must be dropped, got %+v", w)
	}
}

func TestToWireMessages_ImagesBecomeBase64Blocks(t *testing.T) {
	raw := []byte{0x89, 0x50, 0x4e, 0x47}
	w := toWireMessages([]ports.Message{{
		Role: "user", Text: "what is this?",
		Images: []ports.ImagePart{{Format: "PNG", Bytes: raw}},
	}})
	if len(w) != 1 || len(w[0].Content) != 2 {
		t.Fatalf("want text + image, got %+v", w)
	}
	img := w[0].Content[1]
	if img.Type != "image" || img.Source == nil {
		t.Fatalf("block 1 = %+v, want an image", img)
	}
	// Format matching is case-insensitive, and "jpg" maps to Anthropic's "image/jpeg".
	if img.Source.MediaType != "image/png" || img.Source.Type != "base64" {
		t.Errorf("source = %+v", img.Source)
	}
	if img.Source.Data != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("image data is not the base64 of the decoded bytes")
	}
	if jpg := toWireMessages([]ports.Message{{Role: "user",
		Images: []ports.ImagePart{{Format: "jpg", Bytes: raw}}}}); jpg[0].Content[0].Source.MediaType != "image/jpeg" {
		t.Errorf(`"jpg" must map to image/jpeg, got %s`, jpg[0].Content[0].Source.MediaType)
	}
	// An unknown format is dropped: Anthropic rejects the whole request over one bad part,
	// and losing an image beats losing the answer.
	if bad := toWireMessages([]ports.Message{{Role: "user", Text: "t",
		Images: []ports.ImagePart{{Format: "tiff", Bytes: raw}}}}); len(bad[0].Content) != 1 {
		t.Errorf("an unsupported image format must be dropped, got %+v", bad[0].Content)
	}
}

// ── the response side ─────────────────────────────────────────────────────────

func TestInvoke_ToolUseBlockBecomesAToolCall(t *testing.T) {
	res := bufferedInvoke(t, `{"content":[
		{"type":"text","text":"checking"},
		{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{"city":"SP"}}
	],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":20}}`)

	if len(res.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1: %+v", len(res.ToolCalls), res.ToolCalls)
	}
	tc := res.ToolCalls[0]
	if tc.ID != "toolu_9" || tc.Name != "get_weather" {
		t.Errorf("tool call = %+v", tc)
	}
	// Arguments go back as a STRING: that is what the OpenAI dialect the client speaks
	// expects in function.arguments.
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || args["city"] != "SP" {
		t.Errorf("Arguments = %q, want the JSON object as a string", tc.Arguments)
	}
	// stop_reason was dropped entirely before. It is what tells the caller the model stopped
	// to CALL something rather than because it finished.
	if res.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", res.StopReason)
	}
	if res.Text != "checking" {
		t.Errorf("Text = %q", res.Text)
	}
}

// A tool called with no arguments must be "{}" and never "" — several OpenAI SDKs fail to
// parse an empty string in function.arguments.
func TestInvoke_ToolCallWithNoInputIsAnEmptyObject(t *testing.T) {
	res := bufferedInvoke(t, `{"content":[{"type":"tool_use","id":"t","name":"ping"}],
		"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Arguments != "{}" {
		t.Errorf("Arguments = %q, want {}", res.ToolCalls[0].Arguments)
	}
}

// ── streaming ─────────────────────────────────────────────────────────────────

// Anthropic streams tool arguments as FRAGMENTS: the id and name arrive on the block start and
// the arguments follow as input_json_delta. A fragment on its own is not valid JSON, so the
// only correct handling is to accumulate.
func TestAnthropicFrame_ToolCallArgumentsAccumulateAcrossFrames(t *testing.T) {
	var res ports.Result
	anthropicFrame(`{"type":"content_block_start","content_block":{"type":"tool_use","id":"toolu_2","name":"lookup"}}`, &res)
	anthropicFrame(`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"ci"}}`, &res)
	anthropicFrame(`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"ty\":\"SP\"}"}}`, &res)
	anthropicFrame(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`, &res)

	if len(res.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls: %+v", len(res.ToolCalls), res.ToolCalls)
	}
	tc := res.ToolCalls[0]
	if tc.ID != "toolu_2" || tc.Name != "lookup" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.Arguments != `{"city":"SP"}` {
		t.Errorf("Arguments = %q, want the reassembled JSON", tc.Arguments)
	}
	if res.StopReason != "tool_use" {
		t.Errorf("StopReason = %q", res.StopReason)
	}
}

// A streamed tool call that received no fragments must still end as "{}".
func TestAnthropicFrame_StreamedToolCallWithNoFragmentsIsAnEmptyObject(t *testing.T) {
	var res ports.Result
	anthropicFrame(`{"type":"content_block_start","content_block":{"type":"tool_use","id":"t","name":"ping"}}`, &res)
	anthropicFrame(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`, &res)
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Arguments != "{}" {
		t.Errorf("Arguments = %q, want {}", res.ToolCalls[0].Arguments)
	}
}

// Tool argument fragments must not be emitted as prose. They are consumed silently, because
// the client-facing SSE contract does not stream tool arguments today.
func TestAnthropicFrame_ToolFragmentsAreNotReturnedAsContent(t *testing.T) {
	var res ports.Result
	anthropicFrame(`{"type":"content_block_start","content_block":{"type":"tool_use","id":"t","name":"n"}}`, &res)
	c := anthropicFrame(`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`, &res)
	if c.Text != "" || c.Reasoning != "" {
		t.Errorf("a tool fragment must not be emitted as content: %+v", c)
	}
}
