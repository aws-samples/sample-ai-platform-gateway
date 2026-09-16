// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package google is the outbound adapter for Google's native API
// (Generative Language, generateContent). It implements ports.Provider.
//
// rewriting the logic. Content goes as the projected text (Text), the role is
// mapped to user/model, and the system instruction goes in the systemInstruction
// field — exactly as in the original.
package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aiplat/core/internal/ports"
)

// Adapter is a connection bound to a Gemini model.
type Adapter struct {
	HTTP    *http.Client
	BaseURL string
	ModelID string
	APIKey  string
}

var _ ports.Provider = (*Adapter)(nil) // compile-time assertion

func splitSystem(msgs []ports.Message) (string, []ports.Message) {
	var sys []string
	var conv []ports.Message
	for _, m := range msgs {
		if m.Role == "system" {
			if m.Text != "" {
				sys = append(sys, m.Text)
			}
			continue
		}
		conv = append(conv, m)
	}
	return strings.Join(sys, "\n"), conv
}

// geminiImageMediaTypes maps the boundary's bare format onto Gemini's mimeType. An unknown
// format is dropped rather than sent, for the same reason as in the other adapters: the API
// rejects the whole request over one bad part, and losing an image beats losing the answer.
var geminiImageMediaTypes = map[string]string{
	"png":  "image/png",
	"jpeg": "image/jpeg",
	"jpg":  "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
}

// schemaUnsupported are JSON Schema keywords Gemini REJECTS outright.
//
// This is the trap that makes tool use on Gemini look like a gateway bug: a schema that every
// other provider accepts comes back as a 400 naming a field the customer never wrote, because
// their tooling generated it. Stripping the keywords is preferable to forwarding a schema we
// know will be refused — the constraint they express is advisory, and dropping it costs
// validation strictness rather than correctness of the call.
var schemaUnsupported = map[string]bool{
	"$schema":              true,
	"additionalProperties": true,
	"$id":                  true,
	"$ref":                 true,
	"definitions":          true,
	"$defs":                true,
}

// sanitizeSchema removes the unsupported keywords, recursively.
//
// It COPIES rather than editing in place: the schema comes from the client's request and is
// also used to build the cache key, so mutating it here would change a value the caller still
// holds — the same aliasing rule ddbconfig.intersectAllowed documents.
func sanitizeSchema(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			if schemaUnsupported[k] {
				continue
			}
			out[k] = sanitizeSchema(val)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = sanitizeSchema(val)
		}
		return out
	default:
		return v
	}
}

// toFunctionDeclarations converts the boundary's tools into Gemini's shape.
//
// Gemini nests every declaration under ONE `tools` entry with a `functionDeclarations` array —
// not one entry per tool, which it accepts syntactically and then behaves oddly with.
func toFunctionDeclarations(tools []ports.ToolDef) []map[string]interface{} {
	decls := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		d := map[string]interface{}{"name": t.Name}
		if t.Description != "" {
			d["description"] = t.Description
		}
		if t.Parameters != nil {
			d["parameters"] = sanitizeSchema(t.Parameters)
		}
		decls = append(decls, d)
	}
	if len(decls) == 0 {
		return nil
	}
	return []map[string]interface{}{{"functionDeclarations": decls}}
}

// toContents translates the boundary conversation into Gemini `contents`.
//
// Three things were being dropped before, because this projected only `m.Text`:
// images, the model's own function calls when replayed on a later turn, and the function
// RESULTS the client sends back. A tool conversation therefore could not progress past the
// first call — the model asked for a tool, the client answered, and Gemini never saw the
// answer.
//
// The awkward part is that Gemini identifies a function response by NAME while the OpenAI
// dialect identifies it by tool_call_id. The name is recovered from the assistant turn that
// requested it, which is why the ids seen so far are tracked while walking the conversation.
func toContents(msgs []ports.Message) []map[string]interface{} {
	nameByID := map[string]string{}
	var out []map[string]interface{}
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			var parts []map[string]interface{}
			if m.Text != "" {
				parts = append(parts, map[string]interface{}{"text": m.Text})
			}
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					nameByID[tc.ID] = tc.Name
				}
				args := map[string]interface{}{}
				if tc.Arguments != "" {
					json.Unmarshal([]byte(tc.Arguments), &args)
				}
				parts = append(parts, map[string]interface{}{
					"functionCall": map[string]interface{}{"name": tc.Name, "args": args},
				})
			}
			if len(parts) == 0 {
				continue
			}
			out = append(out, map[string]interface{}{"role": "model", "parts": parts})

		case "tool":
			// Gemini keys the response by function name. `name` on the tool message is the
			// OpenAI dialect's own hint; when the client omits it, the name comes from the
			// call this is answering. Without either there is nothing to send — a response
			// with no name is rejected, and guessing would attribute it to the wrong tool.
			name := m.Name
			if name == "" {
				name = nameByID[m.ToolCallID]
			}
			if name == "" {
				continue
			}
			// The result is wrapped in an object: Gemini requires `response` to be a struct,
			// so a bare string is refused. `output` is the conventional key for an
			// unstructured result.
			out = append(out, map[string]interface{}{"role": "user", "parts": []map[string]interface{}{{
				"functionResponse": map[string]interface{}{
					"name":     name,
					"response": map[string]interface{}{"output": m.Text},
				},
			}}})

		default:
			var parts []map[string]interface{}
			if m.Text != "" {
				parts = append(parts, map[string]interface{}{"text": m.Text})
			}
			for _, img := range m.Images {
				mt, ok := geminiImageMediaTypes[strings.ToLower(img.Format)]
				if !ok || len(img.Bytes) == 0 {
					continue
				}
				parts = append(parts, map[string]interface{}{"inlineData": map[string]interface{}{
					"mimeType": mt, "data": base64.StdEncoding.EncodeToString(img.Bytes),
				}})
			}
			if len(parts) == 0 {
				continue
			}
			out = append(out, map[string]interface{}{"role": "user", "parts": parts})
		}
	}
	return out
}

// geminiCallID synthesizes an id for a function call.
//
// Gemini returns NO id — it identifies a call by name only. The OpenAI dialect the client
// speaks requires one on `tool_calls[].id`, and the client echoes it back on the tool result,
// so the value has to be stable within a response and distinguishable between two calls to the
// same tool. Derived from name plus position, which satisfies both. It is explicitly not a
// provider identifier and nothing should treat it as one.
func geminiCallID(name string, idx int) string {
	return fmt.Sprintf("call_%s_%d", name, idx)
}

func (a *Adapter) Invoke(ctx context.Context, in ports.InvokeInput) (ports.Result, error) {
	// Same builder as OpenStream (see stream.go), with stream=false: identical body,
	// different endpoint. Sharing it keeps the two from drifting.
	req, err := a.buildRequest(ctx, in, false)
	if err != nil {
		return ports.Result{}, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return ports.Result{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return ports.Result{}, fmt.Errorf("gemini %d: %s", resp.StatusCode, string(body))
	}
	var d struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
					// `thought: true` marks a part as chain of thought. It is a flag on an
					// ORDINARY text part, not a separate part type.
					Thought          bool   `json:"thought"`
					ThoughtSignature string `json:"thoughtSignature"`
					FunctionCall     *struct {
						Name string          `json:"name"`
						Args json.RawMessage `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount"`
			// Already included in candidatesTokenCount — never added to the totals.
			ThoughtsTokenCount int `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &d); err != nil || len(d.Candidates) == 0 || len(d.Candidates[0].Content.Parts) == 0 {
		return ports.Result{}, fmt.Errorf("bad gemini response")
	}
	res := ports.Result{
		InputTokens:  d.UsageMetadata.PromptTokenCount,
		OutputTokens: d.UsageMetadata.CandidatesTokenCount,
		// In Gemini, cachedContentTokenCount is included in promptTokenCount.
		CacheReadInputTokens: d.UsageMetadata.CachedContentTokenCount,
		CacheCounters:        ports.CacheCountersAbsent,
	}
	// EVERY part, split on the `thought` flag — not Parts[0].
	//
	// Two bugs in one line before this. Taking only the first part truncated any multi-part
	// answer (the streaming path's own comment already called this out). And once
	// thinkingConfig is sent with includeThoughts, part 0 IS the thinking — so the client
	// would have received the model's reasoning presented as the answer, with the real answer
	// discarded.
	for _, p := range d.Candidates[0].Content.Parts {
		if p.ThoughtSignature != "" {
			res.ReasoningSignature = p.ThoughtSignature
		}
		if fc := p.FunctionCall; fc != nil {
			args := "{}"
			if len(fc.Args) > 0 {
				args = string(fc.Args)
			}
			res.ToolCalls = append(res.ToolCalls, ports.ToolCall{
				ID: geminiCallID(fc.Name, len(res.ToolCalls)), Name: fc.Name, Arguments: args,
			})
			continue
		}
		if p.Thought {
			res.Reasoning += p.Text
			continue
		}
		res.Text += p.Text
	}
	res.ReasoningChars = len(res.Reasoning)
	res.StopReason = d.Candidates[0].FinishReason
	if res.CacheReadInputTokens > 0 {
		res.CacheCounters = ports.CacheCountersReported
	}
	return res, nil
}
