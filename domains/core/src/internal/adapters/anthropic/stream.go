// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Native response streaming for Anthropic's Messages API (`"stream": true`).
//
// Without this the adapter returns the complete answer and the gateway slices it into SSE
// frames afterwards, so time-to-first-token equals time-to-last-token. The request is
// built by the SAME code as the buffered path (splitSystem, toWireMessages, the
// cache_control block) — two translations that must agree but are written twice is how a
// streaming request starts quietly differing from a buffered one.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/aiplat/core/internal/adapters/ssestream"
	"github.com/aiplat/core/internal/ports"
)

var _ ports.StreamProvider = (*Adapter)(nil) // compile-time assertion

// OpenStream implements ports.StreamProvider.
//
// It opens and VALIDATES without reading the body, so a failure can still fall back to
// another route: once the caller starts writing frames the HTTP status is already on the
// wire and the only way to report a failure is an in-band error frame.
func (a *Adapter) OpenStream(ctx context.Context, in ports.InvokeInput) (ports.ProviderStream, error) {
	req, err := a.buildRequest(ctx, in, true)
	if err != nil {
		return nil, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic %d: %s", resp.StatusCode, string(b))
	}
	return ssestream.New(resp, anthropicFrame), nil
}

// buildRequest is shared by both paths so the streaming request cannot drift from the
// buffered one. `stream` is the only difference in the body.
func (a *Adapter) buildRequest(ctx context.Context, in ports.InvokeInput, stream bool) (*http.Request, error) {
	baseURL := a.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	system, conv := splitSystem(in.Messages)
	payload := map[string]interface{}{"model": a.ModelID, "max_tokens": 1024, "messages": toWireMessages(conv)}
	if system != "" {
		if a.CachePrefix {
			payload["system"] = []map[string]interface{}{
				{"type": "text", "text": system, "cache_control": map[string]string{"type": "ephemeral"}},
			}
		} else {
			payload["system"] = system
		}
	}
	if stream {
		payload["stream"] = true
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	return req, nil
}

// anthropicFrame interprets one Messages-API stream event.
//
// The usage accounting is split across two events and both halves are needed:
//
//	message_start → input_tokens and the cache counters (and output_tokens: 1, a
//	                placeholder that must NOT be taken as the final count)
//	message_delta → the real output_tokens, plus stop_reason
//
// Reading only one of them is the failure this function exists to prevent: taking
// message_start's output_tokens gives every response a cost of one token, and skipping
// message_start loses the prompt-cache read entirely.
func anthropicFrame(data string, res *ports.Result) string {
	var e struct {
		Type    string `json:"type"`
		Message struct {
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		Delta struct {
			Type       string `json:"type"`
			Text       string `json:"text"`
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage anthropicUsage `json:"usage"`
	}
	if json.Unmarshal([]byte(data), &e) != nil {
		return ""
	}
	switch e.Type {
	case "message_start":
		applyAnthropicUsage(res, e.Message.Usage, false)
	case "content_block_delta":
		// Only text_delta carries visible output. input_json_delta streams tool-call
		// arguments, which the client-facing SSE contract does not expose today, so it is
		// consumed silently rather than being emitted as if it were prose.
		if e.Delta.Type == "text_delta" {
			return e.Delta.Text
		}
	case "message_delta":
		if e.Delta.StopReason != "" {
			res.StopReason = e.Delta.StopReason
		}
		applyAnthropicUsage(res, e.Usage, true)
	}
	return ""
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// applyAnthropicUsage folds a usage block into the Result.
//
// finalOutput distinguishes the two events: message_start's output_tokens is a
// placeholder, so it is ignored there and taken from message_delta.
//
// The reported/absent distinction is preserved exactly as the buffered path computes it.
// It matters because Anthropic's input_tokens EXCLUDES cached tokens, so "the provider
// said zero" and "the provider did not say" price differently.
func applyAnthropicUsage(res *ports.Result, u anthropicUsage, finalOutput bool) {
	if u.InputTokens > 0 {
		res.InputTokens = u.InputTokens
	}
	if finalOutput && u.OutputTokens > 0 {
		res.OutputTokens = u.OutputTokens
	}
	if u.CacheReadInputTokens > 0 {
		res.CacheReadInputTokens = u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens > 0 {
		res.CacheWriteInputTokens = u.CacheCreationInputTokens
	}
	if res.CacheReadInputTokens > 0 || res.CacheWriteInputTokens > 0 {
		res.CacheCounters = ports.CacheCountersReported
	}
}
