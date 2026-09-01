// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

// PROTOCOL shell: emitting SSE in the OpenAI dialect (chat.completion.chunk) and the
// streaming proxy for OpenAI-compatible providers.
//
// A reality note the helper names do not tell: behind API Gateway, SSE comes out
// VALID but BUFFERED (it arrives all at once, not token by token). See the decision
// in aiplat-live-infra.md — a public Function URL is blocked by the account guardrail
// and CloudFront+OAC would require the client to sign the body. Real streaming
// depends on swapping the wrapper (Fargate/App Runner), which is exactly what the
// neutral inbound port (internal/httpapi) unlocks.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func sseWrite(w io.Writer, obj interface{}) {
	b, _ := json.Marshal(obj)
	io.WriteString(w, "data: "+string(b)+"\n\n")
}
func sseRole(w io.Writer, id, model string) {
	sseWrite(w, map[string]interface{}{"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil}}})
}
func sseDelta(w io.Writer, id, model, text string) {
	sseWrite(w, map[string]interface{}{"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]string{"content": text}, "finish_reason": nil}}})
}
func sseStop(w io.Writer, id, model string) {
	sseWrite(w, map[string]interface{}{"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]string{}, "finish_reason": "stop"}}})
	io.WriteString(w, "data: [DONE]\n\n")
}

// pseudoStream: for providers without native streaming here, slices the complete text
// into chunks and emits them as SSE (keeps the client drop-in).
func pseudoStream(w io.Writer, id, model, text string) {
	sseRole(w, id, model)
	const n = 24
	for i := 0; i < len(text); i += n {
		j := i + n
		if j > len(text) {
			j = len(text)
		}
		sseDelta(w, id, model, text[i:j])
	}
	sseStop(w, id, model)
}

// streamOpenAICompat: real streaming, proxying the OpenAI-compatible provider's SSE
// verbatim to the client (it already arrives in the right dialect) while accumulating
// content/usage.
func streamOpenAICompat(ctx context.Context, w io.Writer, baseURL, modelID string, msgs []chatMsg, apiKey string) (string, int, int, error) {
	payload, _ := json.Marshal(map[string]interface{}{"model": modelID, "messages": msgs, "stream": true, "stream_options": map[string]bool{"include_usage": true}})
	req, _ := http.NewRequestWithContext(ctx, "POST", baseURL+"/chat/completions", bytes.NewReader(payload))
	req.Header.Set("content-type", "application/json")
	if apiKey != "" {
		req.Header.Set("authorization", "Bearer "+apiKey)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", 0, 0, fmt.Errorf("provider %d: %s", resp.StatusCode, string(b))
	}
	var content strings.Builder
	tin, tout := 0, 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		io.WriteString(w, line+"\n\n") // pass the frame through to the client
		data := strings.TrimSpace(line[5:])
		if data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) == nil {
			if len(chunk.Choices) > 0 {
				content.WriteString(chunk.Choices[0].Delta.Content)
			}
			if chunk.Usage != nil {
				tin, tout = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
			}
		}
	}
	return content.String(), tin, tout, nil
}
