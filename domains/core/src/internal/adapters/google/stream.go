// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Native response streaming for Gemini (`:streamGenerateContent` with `alt=sse`).
//
// Without this the adapter returns the complete answer and the gateway slices it into SSE
// frames afterwards, so time-to-first-token equals time-to-last-token. The request body is
// built by the SAME code as the buffered path; only the URL differs.
package google

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
// `alt=sse` is not optional. Without it :streamGenerateContent answers with a JSON ARRAY
// delivered in chunks — valid JSON overall, but not line-delimited, so an SSE reader finds
// no `data:` prefix, yields nothing, and the request completes with an empty answer and no
// error anywhere.
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
		return nil, fmt.Errorf("gemini %d: %s", resp.StatusCode, string(b))
	}
	return ssestream.New(resp, geminiFrame), nil
}

// buildRequest is shared by both paths. The body is identical; `stream` selects the
// method and adds alt=sse.
func (a *Adapter) buildRequest(ctx context.Context, in ports.InvokeInput, stream bool) (*http.Request, error) {
	baseURL := a.BaseURL
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	system, conv := splitSystem(in.Messages)
	payload := map[string]interface{}{"contents": toContents(conv)}
	if system != "" {
		payload["systemInstruction"] = map[string]interface{}{"parts": []map[string]string{{"text": system}}}
	}
	// Tools were never sent from here: `in.Tools` was not read at all, so a client sending
	// `tools` to a native Gemini route received prose and no function call. The request
	// succeeded, which is exactly why it went unnoticed.
	if tools := toFunctionDeclarations(in.Tools); len(tools) > 0 {
		payload["tools"] = tools
	}
	// Gemini groups every sampling parameter under one object, and the object is omitted
	// entirely when the client asked for nothing — an empty generationConfig would be
	// accepted but it invites the next reader to start filling it with defaults.
	if gc := generationConfig(in); len(gc) > 0 {
		payload["generationConfig"] = gc
	}
	b, _ := json.Marshal(payload)

	method, extra := ":generateContent?", ""
	if stream {
		method, extra = ":streamGenerateContent?", "alt=sse&"
	}
	url := baseURL + "/v1beta/models/" + a.ModelID + method + extra + "key=" + a.APIKey
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	return req, nil
}

// generationConfig maps the neutral inference parameters onto Gemini's names.
//
// Only what the client actually sent is included. The camelCase keys are not
// interchangeable with the snake_case of the other dialects, and Gemini silently ignores a
// key it does not recognise — so a typo here would not fail, it would just quietly stop
// honouring the parameter, which is the same class of bug as never sending it at all.
func generationConfig(in ports.InvokeInput) map[string]interface{} {
	gc := map[string]interface{}{}
	if in.MaxOutputTokens > 0 {
		gc["maxOutputTokens"] = in.MaxOutputTokens
	}
	if t := in.Inference.Temperature; t != nil {
		gc["temperature"] = *t
	}
	if p := in.Inference.TopP; p != nil {
		gc["topP"] = *p
	}
	if len(in.Inference.Stop) > 0 {
		gc["stopSequences"] = in.Inference.Stop
	}
	if r := in.Inference.Reasoning; r != nil {
		switch {
		case r.Disabled:
			// Gemini is the one provider here that can genuinely be told NOT to think: a
			// zero budget disables it. includeThoughts is set to false with it so the
			// intent is unambiguous rather than relying on the budget alone.
			gc["thinkingConfig"] = map[string]interface{}{"thinkingBudget": 0, "includeThoughts": false}
		case r.BudgetTokens > 0:
			// includeThoughts is what makes the reasoning VISIBLE. Without it Gemini thinks,
			// bills the thinking as output tokens, and returns nothing to show for it — the
			// customer pays for reasoning they cannot read or audit.
			gc["thinkingConfig"] = map[string]interface{}{"thinkingBudget": r.BudgetTokens, "includeThoughts": true}
		}
	}
	return gc
}

// geminiFrame interprets one streamGenerateContent chunk.
//
// Every chunk is a complete GenerateContentResponse, and usageMetadata is CUMULATIVE
// rather than incremental — so the counters are assigned, never added. Adding them would
// inflate the token count (and the cost) by roughly the number of chunks, which for a long
// answer is a large multiple and not an obvious one when reading a bill.
//
// A chunk may also carry several parts; they are concatenated in order. Taking only
// parts[0] — what the buffered path does — silently truncates a multi-part chunk.
func geminiFrame(data string, res *ports.Result) ports.Chunk {
	var d struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
					// `thought: true` is how Gemini marks a part as chain of thought. It is
					// a flag on an ORDINARY text part, not a separate part type — so a
					// reader that ignores it appends the model's thinking straight into the
					// answer. That is worse than dropping it, and it is what would have
					// happened here the moment thinkingConfig started being sent.
					Thought bool `json:"thought"`
					// ThoughtSignature carries the reasoning continuity token when present.
					ThoughtSignature string `json:"thoughtSignature"`
					// Gemini streams a function call WHOLE, in one part — unlike Anthropic
					// and Bedrock, which fragment the arguments. So there is nothing to
					// accumulate here, only to append.
					FunctionCall *struct {
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
			// ThoughtsTokenCount is reported SEPARATELY by Gemini but is already included
			// in candidatesTokenCount, so it must never be added to the totals — doing so
			// would double-charge the thinking.
			ThoughtsTokenCount int `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal([]byte(data), &d) != nil {
		return ports.Chunk{}
	}
	if u := d.UsageMetadata; u.PromptTokenCount > 0 || u.CandidatesTokenCount > 0 {
		res.InputTokens = u.PromptTokenCount
		res.OutputTokens = u.CandidatesTokenCount
		// In Gemini, cachedContentTokenCount is INCLUDED in promptTokenCount — the
		// opposite of Anthropic and Bedrock. The gateway's cost model is told which
		// convention applies through capabilities.cache_tokens_inclusive, so the only job
		// here is to report the number faithfully.
		if u.CachedContentTokenCount > 0 {
			res.CacheReadInputTokens = u.CachedContentTokenCount
			res.CacheCounters = ports.CacheCountersReported
		}
	}
	var out ports.Chunk
	for _, c := range d.Candidates {
		if c.FinishReason != "" {
			res.StopReason = c.FinishReason
		}
		for _, p := range c.Content.Parts {
			if p.ThoughtSignature != "" {
				out.Signature = p.ThoughtSignature
			}
			if fc := p.FunctionCall; fc != nil {
				args := "{}"
				if len(fc.Args) > 0 {
					args = string(fc.Args)
				}
				// Accumulated into the Result rather than returned as a chunk: the
				// client-facing SSE contract does not stream tool arguments today, so a
				// streamed tool call is reported in the final Result, like every other
				// adapter does it.
				res.ToolCalls = append(res.ToolCalls, ports.ToolCall{
					ID: geminiCallID(fc.Name, len(res.ToolCalls)), Name: fc.Name, Arguments: args,
				})
				continue
			}
			// The split is the whole point of reading `thought`: reasoning goes to
			// delta.reasoning_content and answer text to delta.content, never merged.
			if p.Thought {
				out.Reasoning += p.Text
				continue
			}
			out.Text += p.Text
		}
	}
	return out
}
