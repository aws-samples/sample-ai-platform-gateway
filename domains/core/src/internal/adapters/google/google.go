// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package google is the outbound adapter for Google's native API
// (Generative Language, generateContent). It implements ports.Provider.
//
// Feature: hexagonal-refactor, task 3.4. Code MOVED from callGemini without
// rewriting the logic. Content goes as the projected text (Text), the role is
// mapped to user/model, and the system instruction goes in the systemInstruction
// field — exactly as in the original.
package google

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

func (a *Adapter) Invoke(ctx context.Context, in ports.InvokeInput) (ports.Result, error) {
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
	url := baseURL + "/v1beta/models/" + a.ModelID + ":generateContent?key=" + a.APIKey
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	req.Header.Set("content-type", "application/json")
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
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &d); err != nil || len(d.Candidates) == 0 || len(d.Candidates[0].Content.Parts) == 0 {
		return ports.Result{}, fmt.Errorf("bad gemini response")
	}
	res := ports.Result{
		Text:         d.Candidates[0].Content.Parts[0].Text,
		InputTokens:  d.UsageMetadata.PromptTokenCount,
		OutputTokens: d.UsageMetadata.CandidatesTokenCount,
		// In Gemini, cachedContentTokenCount is included in promptTokenCount.
		CacheReadInputTokens: d.UsageMetadata.CachedContentTokenCount,
		CacheCounters:        ports.CacheCountersAbsent,
	}
	if res.CacheReadInputTokens > 0 {
		res.CacheCounters = ports.CacheCountersReported
	}
	return res, nil
}
