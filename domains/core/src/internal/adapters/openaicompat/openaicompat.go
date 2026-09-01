// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package openaicompat is the outbound adapter for providers that speak the OpenAI
// dialect (/chat/completions): OpenAI, Azure, Groq, Together, Gemini-compat and
// self-host. It implements ports.Provider.
//
// Feature: hexagonal-refactor, task 3.2. The wire code was MOVED from
// cmd/router/main.go (callOpenAICompat) without rewriting the logic. The only
// structural change is the boundary: instead of taking []chatMsg (a handler type),
// it takes ports.Message and REBUILDS the wire format from Raw (the original
// content, which preserves multimodal and null) or Text, keeping
// name/tool_calls/tool_call_id.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/aiplat/core/internal/ports"
)

// Adapter is a connection bound to an OpenAI-compatible model/endpoint.
type Adapter struct {
	HTTP    *http.Client
	BaseURL string
	ModelID string
	APIKey  string
}

var _ ports.Provider = (*Adapter)(nil) // compile-time assertion

// wireMsg replicates EXACTLY the shape chatMsg used to marshal onto the wire (same
// tags, same order, same omitempty). That is what guarantees byte equality with the golden.
type wireMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []wireToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description,omitempty"`
		Parameters  map[string]interface{} `json:"parameters,omitempty"`
	} `json:"function"`
}

// toWireMessages rebuilds the wire format from the boundary messages. Raw carries
// the original content (string, null or multimodal array); when absent, it falls
// back to Text. tool_calls come back with type "function" (the only valid value in
// the dialect).
func toWireMessages(msgs []ports.Message) []wireMsg {
	out := make([]wireMsg, len(msgs))
	for i, m := range msgs {
		w := wireMsg{Role: m.Role, Name: m.Name, ToolCallID: m.ToolCallID}
		if len(m.Raw) > 0 {
			w.Content = json.RawMessage(m.Raw)
		} else if m.Text != "" {
			b, _ := json.Marshal(m.Text)
			w.Content = b
		}
		for _, tc := range m.ToolCalls {
			var wc wireToolCall
			wc.ID = tc.ID
			wc.Type = "function"
			wc.Function.Name = tc.Name
			wc.Function.Arguments = tc.Arguments
			w.ToolCalls = append(w.ToolCalls, wc)
		}
		out[i] = w
	}
	return out
}

func toWireTools(tools []ports.ToolDef) []wireToolDef {
	out := make([]wireToolDef, 0, len(tools))
	for _, t := range tools {
		var w wireToolDef
		w.Type = "function"
		w.Function.Name = t.Name
		w.Function.Description = t.Description
		w.Function.Parameters = t.Parameters
		out = append(out, w)
	}
	return out
}

func (a *Adapter) Invoke(ctx context.Context, in ports.InvokeInput) (ports.Result, error) {
	reqBody := map[string]interface{}{"model": a.ModelID, "messages": toWireMessages(in.Messages)}
	if len(in.Tools) > 0 {
		reqBody["tools"] = toWireTools(in.Tools)
	}
	payload, _ := json.Marshal(reqBody)
	// Carries the caller's context so the provider call is cancelled when the
	// request is cancelled or the invocation deadline expires, instead of
	// holding the connection open past the caller's lifetime.
	req, _ := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/chat/completions", bytes.NewReader(payload))
	req.Header.Set("content-type", "application/json")
	if a.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+a.APIKey)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return ports.Result{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return ports.Result{}, fmt.Errorf("provider %d: %s", resp.StatusCode, string(b))
	}
	var d struct {
		Choices []struct {
			Message struct {
				Content   string         `json:"content"`
				ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details,omitempty"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &d); err != nil || len(d.Choices) == 0 {
		return ports.Result{}, fmt.Errorf("bad provider response")
	}
	res := ports.Result{
		Text:          d.Choices[0].Message.Content,
		InputTokens:   d.Usage.PromptTokens,
		OutputTokens:  d.Usage.CompletionTokens,
		ToolCalls:     fromWireToolCalls(d.Choices[0].Message.ToolCalls),
		StopReason:    d.Choices[0].FinishReason,
		CacheCounters: ports.CacheCountersAbsent,
	}
	// OpenAI dialect: cached tokens come in prompt_tokens_details and are ALREADY
	// included in prompt_tokens — hence the "inclusive" convention for this provider.
	if pd := d.Usage.PromptTokensDetails; pd != nil {
		res.CacheReadInputTokens = pd.CachedTokens
		res.CacheCounters = ports.CacheCountersReported
	}
	return res, nil
}

func fromWireToolCalls(tcs []wireToolCall) []ports.ToolCall {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]ports.ToolCall, 0, len(tcs))
	for _, tc := range tcs {
		out = append(out, ports.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return out
}
