// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

// PROTOCOL shell: emitting SSE in the OpenAI dialect (chat.completion.chunk) and the
// streaming proxy for OpenAI-compatible providers.
//
// Frames reach the client progressively (API Gateway response streaming, on by default via
// var.response_streaming). Two asymmetries are worth knowing before reading a latency
// number off this code:
//
//   - every adapter now emits the first frame as soon as the provider does:
//     `openai_compatible` proxies its frames verbatim, and `bedrock` (ConverseStream),
//     `anthropic` (Messages API `stream:true`) and `google` (`:streamGenerateContent`
//     with `alt=sse`) go through ports.StreamProvider. pseudoStream is still the path for
//     a cache hit — where the answer genuinely already exists — and the fallback when a
//     provider refuses to open a stream.
//   - the transport itself is no longer the constraint. aws-lambda-go does not implement
//     the Runtime API side of streaming, so cmd/router runs its own invocation loop; see
//     internal/awslambda/runtimeapi.go.
//
// The two-phase split below (openStreamOpenAICompat + pumpOpenAICompat) exists because
// the HTTP status of the response to the client is fixed before the first payload byte,
// so the fallback chain has to be resolved while nothing has been written yet. That is
// what keeps "every provider failed → 502" honest instead of degrading into a 200
// carrying an error frame.
//
// Frame order on every streaming response, and it is load-bearing:
//
//	role → delta* → stop → final (usage + aiplat) → [DONE]
//
// The terminator is emitted by the CALLER, after accounting, because the cost and savings
// figures do not exist until the stream has been drained. Neither pseudoStream nor
// pumpOpenAICompat writes [DONE] — the provider's own terminator is deliberately swallowed.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aiplat/core/internal/ports"
)

// sseWrite returns the write error so the producers can stop when the consumer is gone.
// Ignoring it means continuing to pull (and pay for) provider tokens that nobody receives.
func sseWrite(w io.Writer, obj interface{}) error {
	b, _ := json.Marshal(obj)
	_, err := io.WriteString(w, "data: "+string(b)+"\n\n")
	return err
}
func sseRole(w io.Writer, id, model string) {
	sseWrite(w, map[string]interface{}{"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil}}})
}
func sseDelta(w io.Writer, id, model, text string) error {
	return sseWrite(w, map[string]interface{}{"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]string{"content": text}, "finish_reason": nil}}})
}
func sseStop(w io.Writer, id, model string) {
	sseWrite(w, map[string]interface{}{"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]string{}, "finish_reason": "stop"}}})
}

// sseFinal emits the LAST frame before the terminator: token usage and the aiplat block.
//
// Why this exists: without it, a streaming request loses the per-request cost, savings and
// cache attribution that the buffered path returns in `aiplat` — which is most of what this
// gateway is for. A caller that turns streaming on would have to give up the numbers, and
// the console would show a bubble with no badges.
//
// It cannot ride on the stop frame. The final figures are only known AFTER the stream has
// been drained (tokens come from the provider's usage frame, cost and savings are computed
// from them), and on the openai_compatible path the stop frame is the provider's own,
// proxied verbatim. So the metadata is a separate frame that goes out once accounting is
// done, immediately before [DONE].
//
// Shape: `choices: []` plus extra top-level fields. That is not an invention — it is what
// OpenAI itself sends for the usage chunk under `stream_options.include_usage`, so clients
// already tolerate a final choice-less frame, and unknown top-level fields are ignored by
// every SDK. Strict consumers that only read `choices[0].delta` see nothing new.
//
// Side benefit: `usage` on a streaming response was missing entirely for the providers that
// do not stream natively. Now every streaming response carries it.
func sseFinal(w io.Writer, id, model string, tin, tout int, meta map[string]interface{}) {
	sseWrite(w, map[string]interface{}{"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]interface{}{},
		"usage":   map[string]int{"prompt_tokens": tin, "completion_tokens": tout, "total_tokens": tin + tout},
		"aiplat":  meta})
	sseDone(w)
}

// sseDone writes the SSE terminator. Separate from sseStop because a frame goes between
// them (see sseFinal): a client stops reading at [DONE], so anything written after it is
// discarded.
func sseDone(w io.Writer) {
	io.WriteString(w, "data: [DONE]\n\n")
}

// pseudoStream: for providers without native streaming here, slices the complete text
// into chunks and emits them as SSE (keeps the client drop-in).
//
// It does NOT terminate the stream. The caller emits sseFinal (or sseDone) after
// accounting, so the metadata frame lands before [DONE].
func pseudoStream(w io.Writer, id, model, text string) {
	sseRole(w, id, model)
	const n = 24
	for i := 0; i < len(text); i += n {
		j := i + n
		if j > len(text) {
			j = len(text)
		}
		if err := sseDelta(w, id, model, text[i:j]); err != nil {
			break
		}
	}
	sseStop(w, id, model)
}

// pumpProviderStream drains a NATIVE provider stream (ports.ProviderStream) into w,
// translating each delta into an OpenAI-dialect frame.
//
// This is the path that makes time-to-first-token real: the first frame goes out as soon as
// the provider emits its first token, instead of after the whole answer exists. pseudoStream
// produces byte-identical output — it just cannot start until the answer is complete.
//
// It returns the provider's own Result rather than loose counters, because a native stream
// reports what pumpOpenAICompat cannot: the stop reason, tool calls, and the prompt-cache
// counters with the reported/absent distinction the cost model depends on.
//
// Like pumpOpenAICompat it returns no error. Once the first frame is out the client is
// committed to a 200, and a stream that breaks halfway still served real tokens: the caller
// bills what arrived instead of discarding it.
func pumpProviderStream(w io.Writer, id, model string, st ports.ProviderStream) ports.Result {
	defer st.Close()
	sseRole(w, id, model)
	for {
		text, err := st.Recv()
		if text != "" {
			if werr := sseDelta(w, id, model, text); werr != nil {
				break // consumer gone; stop pulling provider tokens
			}
		}
		if err != nil {
			break // io.EOF (normal end) or a broken stream — both keep what arrived
		}
	}
	sseStop(w, id, model)
	return st.Result()
}

// openStreamOpenAICompat is PHASE ONE: it opens the provider stream and validates the
// status, WITHOUT writing anything to the client.
//
// That property is the whole point. While this returns an error the caller may still
// try the next route in the chain and, if the chain is exhausted, answer 502 — the
// same contract the buffered path always had. Once phase two starts writing, the status
// line is already on the wire and the only way to report a failure is an in-band frame.
//
// The caller owns closing resp.Body (pumpOpenAICompat does it).
func openStreamOpenAICompat(ctx context.Context, baseURL, modelID string, msgs []chatMsg, apiKey string) (*http.Response, error) {
	payload, _ := json.Marshal(map[string]interface{}{"model": modelID, "messages": msgs, "stream": true, "stream_options": map[string]bool{"include_usage": true}})
	req, _ := http.NewRequestWithContext(ctx, "POST", baseURL+"/chat/completions", bytes.NewReader(payload))
	req.Header.Set("content-type", "application/json")
	if apiKey != "" {
		req.Header.Set("authorization", "Bearer "+apiKey)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("provider %d: %s", resp.StatusCode, string(b))
	}
	return resp, nil
}

// pumpOpenAICompat is PHASE TWO: it drains an already-open provider stream into w,
// proxying each SSE frame verbatim (the provider already speaks the right dialect)
// while accumulating content and usage for the ledger.
//
// It returns no error: by this point the client is committed to a 200 and the frames
// already delivered are real. A truncated stream (provider hung up, client
// disconnected) yields whatever was accumulated, so the Usage_Record still reports the
// tokens that were actually served instead of nothing.
func pumpOpenAICompat(w io.Writer, resp *http.Response) (string, int, int) {
	defer resp.Body.Close()
	var content strings.Builder
	tin, tout := 0, 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[5:])
		// The provider's terminator is SWALLOWED, not forwarded: the caller emits its own
		// after the metadata frame (see sseFinal). Forwarding this one would end the
		// stream for the client before the cost and savings figures were written, and the
		// client would silently never see them.
		if data == "[DONE]" {
			continue
		}
		// Pass the frame through to the client. A write error means the consumer is
		// gone (client disconnected, or the runtime closed the pipe): stop pumping and
		// let the caller record what was served up to here. Continuing would burn the
		// rest of the provider's tokens with nobody to receive them.
		if _, werr := io.WriteString(w, line+"\n\n"); werr != nil {
			return content.String(), tin, tout
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
	return content.String(), tin, tout
}
