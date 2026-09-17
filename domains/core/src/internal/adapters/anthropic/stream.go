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
		return nil, fmt.Errorf("%s %d: %s", a.Transport.label(), resp.StatusCode, string(b))
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
	// max_tokens is REQUIRED by the Messages API, so unlike every other parameter it needs
	// a fallback rather than being omitted. It used to be hardcoded to 1024 with no way for
	// a caller to change it, which silently truncated any answer longer than that — the
	// client's own max_tokens was parsed by the gateway and thrown away before it got here.
	maxTok := in.MaxOutputTokens
	if maxTok <= 0 {
		maxTok = DefaultMaxTokens
	}
	payload := map[string]interface{}{"model": a.ModelID, "max_tokens": maxTok, "messages": toWireMessages(conv)}
	// The Bedrock family carries the API version in the BODY and rejects the request
	// outright without it. Anthropic's own API carries it in a header instead, so this is
	// set only when the transport asks for it — sending both would be sending a field
	// Anthropic does not declare.
	if v := a.Transport.BodyVersion; v != "" {
		payload["anthropic_version"] = v
	}
	// Conditional, all of them: an absent field lets the model's own default stand, while a
	// zero we invented would change the output. temperature 0 is a real request for
	// determinism and must stay distinguishable from "not informed".
	if t := in.Inference.Temperature; t != nil {
		payload["temperature"] = *t
	}
	if p := in.Inference.TopP; p != nil {
		payload["top_p"] = *p
	}
	if len(in.Inference.Stop) > 0 {
		payload["stop_sequences"] = in.Inference.Stop
	}
	// Extended thinking, in whichever shape THIS model accepts (see ReasoningStyle*).
	applyThinking(payload, in.Inference.Reasoning, a.Transport.ReasoningStyle)
	// Tools. `in.Tools` was never read here at all, so a client sending `tools` to a native
	// Anthropic route got an ordinary prose answer and no tool call — the request succeeded,
	// which is what made it invisible. The routing layer had already checked the model was
	// tool-capable, so the only thing missing was sending them.
	if tools := toWireTools(in.Tools); len(tools) > 0 {
		payload["tools"] = tools
	}
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
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+a.Transport.path(), bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	// Auth is either a key or a signature, never both. Signing must be the LAST thing done
	// to the request: SigV4 covers the headers it signs, so setting one afterwards
	// invalidates the signature — which surfaces as a 403 that looks like bad credentials.
	if sign := a.Transport.Sign; sign != nil {
		if err := sign(ctx, req, b); err != nil {
			return nil, err
		}
		return req, nil
	}
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	return req, nil
}

// applyThinking writes the extended-thinking request in the shape the model accepts.
//
// Nothing is written when the client did not ask to think, or asked NOT to: the absence of
// the field is what "do not think" looks like on both shapes, and inventing a disabled
// marker would be sending a field neither API documents.
func applyThinking(payload map[string]interface{}, r *ports.ReasoningRequest, style string) {
	if r == nil || r.Disabled {
		return
	}
	if style == ReasoningStyleAdaptive {
		// Adaptive lets the MODEL choose the budget, so the token count the gateway
		// resolved is not sent at all — there is no field for it. What this shape takes is
		// the client's word, which ReasoningRequest carries alongside the budget precisely
		// for the dialects that want it back.
		payload["thinking"] = map[string]interface{}{"type": "adaptive"}
		// Effort is omitted rather than derived from BudgetTokens when the client sent only
		// a number. Deriving it would mean a second, inverse copy of the gateway's
		// effort→budget table, and the two would drift; adaptive alone is a valid request
		// (measured: opus-4-8 produced a 45-token thinking block with no effort named).
		switch r.Effort {
		case ports.ReasoningEffortLow, ports.ReasoningEffortMedium, ports.ReasoningEffortHigh:
			payload["output_config"] = map[string]interface{}{"effort": r.Effort}
		}
		return
	}
	// Budget shape. The API's own invariant is max_tokens > budget_tokens, which is why the
	// payload is built from a maxTok the gateway has already reconciled with the budget.
	if r.BudgetTokens > 0 {
		payload["thinking"] = map[string]interface{}{"type": "enabled", "budget_tokens": r.BudgetTokens}
	}
}

// DefaultMaxTokens is the fallback for a request that names no ceiling.
//
// The Messages API rejects a request without max_tokens, so something has to be sent. 4096
// rather than the 1024 that used to be hardcoded: 1024 truncates ordinary prose answers,
// and a caller has no way to tell a truncated answer from a complete one except by reading
// it. A client that cares sends its own value, which now actually reaches the provider.
// Exported so a test outside this package can assert against the value rather than repeat
// the number — a duplicated literal is a test that keeps passing after the default changes.
const DefaultMaxTokens = 4096

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
func anthropicFrame(data string, res *ports.Result) ports.Chunk {
	var e struct {
		Type    string `json:"type"`
		Message struct {
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		Delta struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
			// type=input_json_delta: one FRAGMENT of the tool arguments. A fragment on its
			// own is not valid JSON, so it can only be appended.
			PartialJSON string `json:"partial_json"`
			// Anthropic sends redacted thinking as a whole content block rather than a
			// delta; `data` here is the encrypted payload, which is never readable and is
			// only ever used as a marker.
			Data       string `json:"data"`
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
			Data string `json:"data"`
			// type=tool_use: the id and name arrive on the START event; the arguments
			// follow as input_json_delta fragments.
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Usage anthropicUsage `json:"usage"`
	}
	if json.Unmarshal([]byte(data), &e) != nil {
		return ports.Chunk{}
	}
	switch e.Type {
	case "message_start":
		applyAnthropicUsage(res, e.Message.Usage, false)
	case "content_block_start":
		// A redacted thinking block has no deltas to follow — the encrypted payload arrives
		// on the start event. Recording it here is what keeps "the model did not think"
		// distinguishable from "we were not allowed to see it".
		if e.ContentBlock.Type == "redacted_thinking" {
			return ports.Chunk{Redacted: true}
		}
		// A tool call opens here with its id and name, and its arguments arrive as fragments
		// afterwards. Recording it now — with empty arguments — is what gives the fragments
		// somewhere to accumulate; the Result is carried across frames for exactly this.
		if e.ContentBlock.Type == "tool_use" {
			res.ToolCalls = append(res.ToolCalls, ports.ToolCall{ID: e.ContentBlock.ID, Name: e.ContentBlock.Name})
		}
	case "content_block_delta":
		// Each delta type goes to its OWN field. thinking_delta must never be returned as
		// Text: every OpenAI SDK concatenates delta.content, so blending them would print
		// the model's private reasoning inside the answer an end user reads.
		//
		// input_json_delta streams tool-call arguments, which the client-facing SSE
		// contract does not expose today, so it stays consumed silently rather than being
		// emitted as if it were prose.
		switch e.Delta.Type {
		case "text_delta":
			return ports.Chunk{Text: e.Delta.Text}
		case "thinking_delta":
			return ports.Chunk{Reasoning: e.Delta.Thinking}
		case "signature_delta":
			// Opaque metadata, never content. Kept because it is the only thing that makes
			// this reasoning chain continuable on a later turn.
			return ports.Chunk{Signature: e.Delta.Signature}
		case "input_json_delta":
			// Appended to the tool call opened by content_block_start. Nothing is returned:
			// the client-facing SSE contract does not stream tool arguments today, so these
			// are accumulated for the final Result and consumed silently rather than being
			// emitted as if they were prose.
			if n := len(res.ToolCalls); n > 0 {
				res.ToolCalls[n-1].Arguments += e.Delta.PartialJSON
			}
		}
	case "message_delta":
		if e.Delta.StopReason != "" {
			res.StopReason = e.Delta.StopReason
		}
		applyAnthropicUsage(res, e.Usage, true)
		// The last frame that carries state, so this is where the accumulated fragments are
		// finalized. A tool called with no arguments must go out as "{}" and never as the
		// empty string — several OpenAI SDKs fail to parse "" in function.arguments.
		for i := range res.ToolCalls {
			if res.ToolCalls[i].Arguments == "" {
				res.ToolCalls[i].Arguments = "{}"
			}
		}
	}
	return ports.Chunk{}
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
