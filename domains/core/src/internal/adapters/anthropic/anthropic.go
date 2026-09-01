// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package anthropic is the outbound adapter for Anthropic's native API
// (Messages API), its own dialect. It implements ports.Provider.
//
// Feature: hexagonal-refactor, task 3.3. Code MOVED from callAnthropic without
// rewriting the logic. Non-system messages go in the body with the same shape
// chatMsg used to marshal (content raw/null/array, name, tool_calls, tool_call_id),
// rebuilt from ports.Message; system messages are concatenated into the dedicated
// `system` field, as in the original.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aiplat/core/internal/ports"
)

// Adapter is a connection bound to an Anthropic model.
type Adapter struct {
	HTTP    *http.Client
	BaseURL string
	ModelID string
	APIKey  string

	// CachePrefix turns on the provider's prompt caching: when true, the adapter
	// sends `system` in the block format with `cache_control: ephemeral` at the end,
	// telling Anthropic to cache the stable prefix (system). It is the same
	// per-route opt-in as Bedrock's (config.routing[model].prompt_cache) — the
	// cache-write charges a premium and only pays off when the prefix repeats.
	CachePrefix bool
}

var _ ports.Provider = (*Adapter)(nil) // compile-time assertion

// wireMsg replicates the shape chatMsg used to marshal (same tags/order/omitempty).
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

// splitSystem separates system messages (concatenated) from the conversation. Same
// logic as the original, now over ports.Message (Text is the textual projection of content).
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

func (a *Adapter) Invoke(ctx context.Context, in ports.InvokeInput) (ports.Result, error) {
	baseURL := a.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	system, conv := splitSystem(in.Messages)
	payload := map[string]interface{}{"model": a.ModelID, "max_tokens": 1024, "messages": toWireMessages(conv)}
	if system != "" {
		// With prompt caching on, system goes as a block with cache_control
		// ephemeral (marking the end of the stable prefix to cache). Without it, it
		// stays a plain string — keeping the wire identical to before for routes
		// without caching.
		if a.CachePrefix {
			payload["system"] = []map[string]interface{}{
				{"type": "text", "text": system, "cache_control": map[string]string{"type": "ephemeral"}},
			}
		} else {
			payload["system"] = system
		}
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", baseURL+"/v1/messages", bytes.NewReader(b))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return ports.Result{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return ports.Result{}, fmt.Errorf("anthropic %d: %s", resp.StatusCode, string(body))
	}
	var d struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &d); err != nil || len(d.Content) == 0 {
		return ports.Result{}, fmt.Errorf("bad anthropic response")
	}
	res := ports.Result{
		Text: d.Content[0].Text, InputTokens: d.Usage.InputTokens, OutputTokens: d.Usage.OutputTokens,
		CacheReadInputTokens: d.Usage.CacheReadInputTokens, CacheWriteInputTokens: d.Usage.CacheCreationInputTokens,
		CacheCounters: ports.CacheCountersAbsent,
	}
	// In Anthropic's native API, input_tokens EXCLUDES the cached ones: they come in
	// dedicated fields. Hence this provider's convention is exclusive.
	if res.CacheReadInputTokens > 0 || res.CacheWriteInputTokens > 0 {
		res.CacheCounters = ports.CacheCountersReported
	}
	return res, nil
}
