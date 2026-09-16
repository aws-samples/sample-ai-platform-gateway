// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package google

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aiplat/core/internal/ports"
)

// ── the defect ────────────────────────────────────────────────────────────────
//
// buildRequest projected ONLY `m.Text`, and `in.Tools` was never read. So three things were
// dropped silently: tools (a client sending them got prose and no function call), images (a
// multimodal request that passed the eligibility filter arrived text-only), and function
// RESULTS — which means a tool conversation could not progress past the first call: the model
// asked for a tool, the client answered, and Gemini never saw the answer.

func TestToFunctionDeclarations_NestsUnderOneToolsEntry(t *testing.T) {
	got := toFunctionDeclarations([]ports.ToolDef{
		{Name: "a", Description: "first", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "b"},
	})
	// Gemini nests EVERY declaration under one `tools` entry with a functionDeclarations
	// array — not one entry per tool.
	if len(got) != 1 {
		t.Fatalf("want a single tools entry, got %d: %v", len(got), got)
	}
	decls, ok := got[0]["functionDeclarations"].([]map[string]interface{})
	if !ok || len(decls) != 2 {
		t.Fatalf("functionDeclarations = %v", got[0])
	}
	if decls[0]["name"] != "a" || decls[0]["description"] != "first" {
		t.Errorf("declaration 0 = %v", decls[0])
	}
	if _, hasParams := decls[1]["parameters"]; hasParams {
		t.Error("a tool with no parameters must not send an empty `parameters`")
	}
	if toFunctionDeclarations(nil) != nil {
		t.Error("no tools must mean no tools key")
	}

	// The schema must be SANITIZED on the way out, not merely sanitizable. Asserting
	// sanitizeSchema in isolation leaves this call site uncovered — the helper is correct and
	// simply never called, which is the failure mode that keeps a suite green.
	withJunk := toFunctionDeclarations([]ports.ToolDef{{Name: "t", Parameters: map[string]interface{}{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
	}}})
	params := withJunk[0]["functionDeclarations"].([]map[string]interface{})[0]["parameters"].(map[string]interface{})
	for _, k := range []string{"$schema", "additionalProperties"} {
		if _, present := params[k]; present {
			t.Errorf("%q reached the declaration; Gemini rejects it and the 400 names a field the customer never wrote", k)
		}
	}
	if params["type"] != "object" {
		t.Errorf("sanitizing must keep the supported keywords: %v", params)
	}
}

// The trap that makes tool use on Gemini look like a gateway bug: Gemini REJECTS JSON Schema
// keywords that every other provider accepts, and a customer's tooling generates them without
// being asked. The 400 then names a field they never wrote.
func TestSanitizeSchema_StripsWhatGeminiRejects(t *testing.T) {
	in := map[string]interface{}{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"nested": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties":           map[string]interface{}{"x": map[string]interface{}{"type": "string"}},
			},
			"list": []interface{}{
				map[string]interface{}{"additionalProperties": true, "type": "string"},
			},
		},
	}
	out := sanitizeSchema(in).(map[string]interface{})

	if _, bad := out["$schema"]; bad {
		t.Error("$schema must be stripped")
	}
	if _, bad := out["additionalProperties"]; bad {
		t.Error("additionalProperties must be stripped at the top level")
	}
	if out["type"] != "object" {
		t.Error("supported keywords must survive")
	}
	props := out["properties"].(map[string]interface{})
	nested := props["nested"].(map[string]interface{})
	if _, bad := nested["additionalProperties"]; bad {
		t.Error("stripping must be RECURSIVE — a nested schema is where tooling puts it")
	}
	if nested["properties"].(map[string]interface{})["x"].(map[string]interface{})["type"] != "string" {
		t.Error("nested supported keywords must survive")
	}
	inList := props["list"].([]interface{})[0].(map[string]interface{})
	if _, bad := inList["additionalProperties"]; bad {
		t.Error("stripping must reach inside arrays too")
	}

	// It must COPY, never edit in place: this schema came from the client's request and is
	// also used to build the cache key, so mutating it would change a value the caller holds.
	if _, gone := in["additionalProperties"]; !gone {
		t.Error("the input schema was mutated; sanitizeSchema must copy")
	}
	if _, gone := in["$schema"]; !gone {
		t.Error("the input schema was mutated; sanitizeSchema must copy")
	}
}

func TestToContents_AssistantToolCallsBecomeFunctionCallParts(t *testing.T) {
	got := toContents([]ports.Message{{
		Role: "assistant", Text: "looking",
		ToolCalls: []ports.ToolCall{{ID: "c1", Name: "get_weather", Arguments: `{"city":"SP"}`}},
	}})
	if len(got) != 1 || got[0]["role"] != "model" {
		t.Fatalf("an assistant turn must map to role model: %v", got)
	}
	parts := got[0]["parts"].([]map[string]interface{})
	if len(parts) != 2 || parts[0]["text"] != "looking" {
		t.Fatalf("parts = %v", parts)
	}
	fc, ok := parts[1]["functionCall"].(map[string]interface{})
	if !ok {
		t.Fatalf("part 1 is not a functionCall: %v", parts[1])
	}
	if fc["name"] != "get_weather" {
		t.Errorf("functionCall name = %v", fc["name"])
	}
	// Gemini takes args as a STRUCT, not the dialect's JSON string.
	if args, ok := fc["args"].(map[string]interface{}); !ok || args["city"] != "SP" {
		t.Errorf("args = %v, want a parsed object", fc["args"])
	}
}

// Gemini keys a function response by NAME while the OpenAI dialect keys it by tool_call_id, so
// the name has to be recovered from the assistant turn that requested it. Without this, a tool
// conversation stalls: the response is unsendable and gets dropped.
func TestToContents_FunctionResponseNameComesFromThePrecedingCall(t *testing.T) {
	got := toContents([]ports.Message{
		{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "c1", Name: "get_weather", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "c1", Text: "22C"}, // no Name — the client omitted it
	})
	if len(got) != 2 {
		t.Fatalf("want the model turn and the response, got %v", got)
	}
	parts := got[1]["parts"].([]map[string]interface{})
	fr, ok := parts[0]["functionResponse"].(map[string]interface{})
	if !ok {
		t.Fatalf("not a functionResponse: %v", parts[0])
	}
	if fr["name"] != "get_weather" {
		t.Errorf("name = %v, want it resolved from the call id", fr["name"])
	}
	// `response` must be a STRUCT: Gemini refuses a bare string.
	resp, ok := fr["response"].(map[string]interface{})
	if !ok {
		t.Fatalf("response must be an object, got %T", fr["response"])
	}
	if resp["output"] != "22C" {
		t.Errorf("response = %v", resp)
	}
}

// An explicit `name` on the tool message wins — it is the dialect's own hint.
func TestToContents_ExplicitToolNameIsUsed(t *testing.T) {
	got := toContents([]ports.Message{{Role: "tool", Name: "explicit", ToolCallID: "x", Text: "r"}})
	fr := got[0]["parts"].([]map[string]interface{})[0]["functionResponse"].(map[string]interface{})
	if fr["name"] != "explicit" {
		t.Errorf("name = %v, want the explicit one", fr["name"])
	}
}

// With neither a name nor a resolvable id there is nothing to send: a response with no name is
// rejected, and guessing would attribute the result to the wrong tool.
func TestToContents_UnresolvableToolResponseIsDropped(t *testing.T) {
	if got := toContents([]ports.Message{{Role: "tool", ToolCallID: "unknown", Text: "r"}}); len(got) != 0 {
		t.Errorf("an unresolvable function response must be dropped, got %v", got)
	}
}

func TestToContents_ImagesBecomeInlineData(t *testing.T) {
	raw := []byte{1, 2, 3}
	got := toContents([]ports.Message{{
		Role: "user", Text: "what is this?",
		Images: []ports.ImagePart{{Format: "PNG", Bytes: raw}},
	}})
	parts := got[0]["parts"].([]map[string]interface{})
	if len(parts) != 2 {
		t.Fatalf("want text + image, got %v", parts)
	}
	inline, ok := parts[1]["inlineData"].(map[string]interface{})
	if !ok {
		t.Fatalf("part 1 is not inlineData: %v", parts[1])
	}
	if inline["mimeType"] != "image/png" {
		t.Errorf("mimeType = %v", inline["mimeType"])
	}
	if inline["data"] != base64.StdEncoding.EncodeToString(raw) {
		t.Error("inlineData must carry the base64 of the decoded bytes")
	}
	// Unsupported formats are dropped rather than failing the request.
	bad := toContents([]ports.Message{{Role: "user", Text: "t",
		Images: []ports.ImagePart{{Format: "tiff", Bytes: raw}}}})
	if len(bad[0]["parts"].([]map[string]interface{})) != 1 {
		t.Error("an unsupported image format must be dropped")
	}
}

// A plain conversation must produce exactly what it produced before: the fix is additive.
func TestToContents_PlainConversationUnchanged(t *testing.T) {
	got := toContents([]ports.Message{
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: "hello"},
	})
	want := []map[string]interface{}{
		{"role": "user", "parts": []map[string]interface{}{{"text": "hi"}}},
		{"role": "model", "parts": []map[string]interface{}{{"text": "hello"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

// ── the response side ─────────────────────────────────────────────────────────

func TestInvoke_FunctionCallBecomesAToolCall(t *testing.T) {
	res := bufferedInvoke(t, `{"candidates":[{"content":{"parts":[
		{"text":"checking"},
		{"functionCall":{"name":"get_weather","args":{"city":"SP"}}}
	]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":6}}`)

	if len(res.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls: %+v", len(res.ToolCalls), res.ToolCalls)
	}
	tc := res.ToolCalls[0]
	if tc.Name != "get_weather" {
		t.Errorf("name = %q", tc.Name)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || args["city"] != "SP" {
		t.Errorf("Arguments = %q, want the object as a string", tc.Arguments)
	}
	// Gemini returns NO id, and the OpenAI dialect requires one on tool_calls[].id — the
	// client echoes it back on the result, so it has to be non-empty and stable.
	if tc.ID == "" {
		t.Error("a synthesized call id is required; the client echoes it back")
	}
	if res.Text != "checking" {
		t.Errorf("Text = %q, want the text part only", res.Text)
	}
	if res.StopReason != "STOP" {
		t.Errorf("StopReason = %q", res.StopReason)
	}
}

// Two calls to the SAME tool must not collide: the client keys its results by id.
func TestInvoke_TwoCallsToTheSameToolGetDistinctIDs(t *testing.T) {
	res := bufferedInvoke(t, `{"candidates":[{"content":{"parts":[
		{"functionCall":{"name":"lookup","args":{"q":"a"}}},
		{"functionCall":{"name":"lookup","args":{"q":"b"}}}
	]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`)
	if len(res.ToolCalls) != 2 {
		t.Fatalf("got %d calls", len(res.ToolCalls))
	}
	if res.ToolCalls[0].ID == res.ToolCalls[1].ID {
		t.Errorf("both calls got the id %q; results would be indistinguishable", res.ToolCalls[0].ID)
	}
}

func TestGeminiFrame_StreamedFunctionCallIsCollected(t *testing.T) {
	var res ports.Result
	// Gemini streams a function call WHOLE, in one part — unlike Anthropic, which fragments
	// the arguments. So there is nothing to accumulate.
	c := geminiFrame(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"n","args":{"a":1}}}]}}]}`, &res)
	if c.Text != "" || c.Reasoning != "" {
		t.Errorf("a function call must not be emitted as content: %+v", c)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "n" {
		t.Fatalf("tool calls = %+v", res.ToolCalls)
	}
	if res.ToolCalls[0].Arguments != `{"a":1}` {
		t.Errorf("Arguments = %q", res.ToolCalls[0].Arguments)
	}
}
