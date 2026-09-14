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
	var contents []map[string]interface{}
	for _, m := range conv {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]interface{}{"role": role, "parts": []map[string]string{{"text": m.Text}}})
	}
	payload := map[string]interface{}{"contents": contents}
	if system != "" {
		payload["systemInstruction"] = map[string]interface{}{"parts": []map[string]string{{"text": system}}}
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

// geminiFrame interprets one streamGenerateContent chunk.
//
// Every chunk is a complete GenerateContentResponse, and usageMetadata is CUMULATIVE
// rather than incremental — so the counters are assigned, never added. Adding them would
// inflate the token count (and the cost) by roughly the number of chunks, which for a long
// answer is a large multiple and not an obvious one when reading a bill.
//
// A chunk may also carry several parts; they are concatenated in order. Taking only
// parts[0] — what the buffered path does — silently truncates a multi-part chunk.
func geminiFrame(data string, res *ports.Result) string {
	var d struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal([]byte(data), &d) != nil {
		return ""
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
	text := ""
	for _, c := range d.Candidates {
		if c.FinishReason != "" {
			res.StopReason = c.FinishReason
		}
		for _, p := range c.Content.Parts {
			text += p.Text
		}
	}
	return text
}
