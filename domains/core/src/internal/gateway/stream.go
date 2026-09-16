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
	"sync"
	"time"

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

// sseReasoning emits a chain-of-thought delta.
//
// The field is `reasoning_content`, SEPARATE from `content`, and that separation is the
// whole correctness argument: a client that concatenates `delta.content` — which is every
// OpenAI SDK — must not end up with the model's thinking inside the answer. A client that
// does not know the field ignores it, which is the correct default for an extension.
//
// The name follows the convention other gateways settled on for the chat-completions
// dialect rather than being invented here, so an existing client that already renders
// reasoning works without changes.
//
// This is also what closes the "long think" gap: an extended-thinking model streams its
// reasoning during the think phase, so forwarding it means bytes flow while the model
// thinks. The keepalive comment frame remains the safety net for models that expose no
// reasoning at all.
func sseReasoning(w io.Writer, id, model, text string) error {
	return sseWrite(w, map[string]interface{}{"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]string{"reasoning_content": text}, "finish_reason": nil}}})
}

// ssePing writes an SSE COMMENT frame (a line starting with ':'), which the SSE spec tells
// clients to ignore. It is not a chunk: it carries no id, no choices, and never appears in
// the message stream, so it cannot be mistaken for content or disturb the frame contract.
//
// It exists because idle timeouts are counted on bytes, not on progress. API Gateway cuts a
// streaming response after 5 minutes of silence on a Regional endpoint (30 seconds on
// edge-optimized), and any proxy in front of the client has its own idle timer — 60 seconds
// is a common default. A model that thinks for minutes without emitting anything trips those
// before the first token, so the connection dies on a request that was working.
//
// Deliberately excluded from every counter: a ping is not a token, has no cost, and must not
// appear in the frame count of the logs. Instrumentation that counts it would report traffic
// that never existed.
func ssePing(w io.Writer) error {
	_, err := io.WriteString(w, ": ping\n\n")
	return err
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

// pseudoStream: for providers without native streaming here, slices the complete answer
// into chunks and emits them as SSE (keeps the client drop-in).
//
// It does NOT terminate the stream. The caller emits sseFinal (or sseDone) after
// accounting, so the metadata frame lands before [DONE].
//
// `reasoning` is emitted FIRST, as reasoning_content frames, and only then the answer. That
// ordering matches what a native reasoning stream produces, which is the point: without it,
// `reasoning_content` would reach a streaming client on every provider EXCEPT one served
// through this fallback — the reasoning would sit in the ledger, already billed, and never be
// shown. Same argument applies to replaying a cached answer that contains reasoning.
func pseudoStream(w io.Writer, id, model, reasoning, text string) {
	sseRole(w, id, model)
	const n = 24
	slice := func(s string, emit func(io.Writer, string, string, string) error) bool {
		for i := 0; i < len(s); i += n {
			j := i + n
			if j > len(s) {
				j = len(s)
			}
			if err := emit(w, id, model, s[i:j]); err != nil {
				return false
			}
		}
		return true
	}
	if slice(reasoning, sseReasoning) {
		slice(text, sseDelta)
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
// It also owns two things that only make sense here, because this is the single place that
// writes to the client during a stream:
//
//   - the KEEPALIVE comment frame, emitted after `keepalive` of silence (0 disables it);
//   - the DEADLINE, via ctx: when it expires the pump stops and the caller closes the
//     stream normally, so the client gets stop → final → [DONE] instead of a connection
//     that simply dies mid-answer.
//
// The provider read runs in a goroutine and the pump selects over it. That is not for
// concurrency — it is because Recv BLOCKS, and a blocking read cannot coexist with a timer
// in the same goroutine. The write side stays here: exactly one goroutine ever writes to w,
// which is what keeps interleaved frames impossible.
func pumpProviderStream(ctx context.Context, w io.Writer, id, model string, st ports.ProviderStream, keepalive time.Duration) ports.Result {
	defer st.Close()
	sseRole(w, id, model)

	next := chunkReader(st)
	type recvd struct {
		c   ports.Chunk
		err error
	}
	ch := make(chan recvd, 1)
	done := make(chan struct{})
	defer close(done)

	// The Result is SNAPSHOT by the reader goroutine and published under this mutex, rather
	// than read from the stream at the end.
	//
	// Reading st.Result() after the pump gives up is a data race, and one that only appears
	// on the paths that matter least often and hurt most: on a deadline or a dead consumer
	// the reader can still be blocked inside the adapter, and cancelling ctx here does not
	// cancel the provider's HTTP stream (it was opened with the request context), so that
	// read can complete and mutate the adapter's accumulated Result while this goroutine
	// reads it. Snapshotting keeps the adapter touched by exactly one goroutine: the reader
	// reads back what only it wrote, and the pump only ever sees the published copy.
	//
	// The snapshot is also the best answer available on an abandoned stream — it holds the
	// counters as of the last chunk that did arrive, which is what has to be billed.
	var mu sync.Mutex
	snap := st.Result() // pre-stream state, so an immediate abandon still returns a valid zero
	go func() {
		for {
			c, err := next()
			mu.Lock()
			snap = st.Result()
			mu.Unlock()
			select {
			case ch <- recvd{c, err}:
			case <-done:
				return // pump gave up (deadline, client gone); do not leak on the send
			}
			if err != nil {
				return
			}
		}
	}()

	// A ticker rather than a timer reset per write: resetting from the select arm races
	// with a read already in flight, and the condition below is cheaper to reason about.
	var tick <-chan time.Time
	if keepalive > 0 {
		t := time.NewTicker(keepalive)
		defer t.Stop()
		tick = t.C
	}
	last := time.Now()

	for streaming := true; streaming; {
		select {
		case r := <-ch:
			// Reasoning, signature and redacted markers all count as activity even when
			// nothing is forwarded — the point of tracking activity is the idle timer, and
			// bytes did arrive from the provider.
			last = time.Now()
			if r.c.Text != "" {
				if werr := sseDelta(w, id, model, r.c.Text); werr != nil {
					streaming = false // consumer gone; stop pulling provider tokens
					break
				}
			}
			if r.c.Reasoning != "" {
				if werr := sseReasoning(w, id, model, r.c.Reasoning); werr != nil {
					streaming = false
					break
				}
			}
			if r.err != nil {
				streaming = false // io.EOF (normal end) or a broken stream — keep what arrived
			}
		case <-tick:
			if time.Since(last) < keepalive {
				continue // real traffic is flowing; a ping would be noise
			}
			if werr := ssePing(w); werr != nil {
				streaming = false
			}
			last = time.Now()
		case <-ctx.Done():
			// Deadline or cancellation. Everything served so far is real and billed, so
			// the caller still accounts for it; falling through to sseStop below is what
			// makes the client see a well-formed end instead of a truncated stream.
			streaming = false
		}
	}
	sseStop(w, id, model)
	mu.Lock()
	res := snap
	mu.Unlock()
	return res
}

// streamDeadlineMargin is time reserved AFTER the pump stops, for the work that still has to
// happen: the stop frame, accounting, the final metadata frame, [DONE] and the usage record.
// Without it a request that runs to the runtime's limit loses exactly the numbers this
// gateway exists to produce — the tokens were billed by the provider and nothing recorded
// them.
const streamDeadlineMargin = 5 * time.Second

// defaultSSEKeepalive is the silence tolerated before a ping when no scope configured one.
//
// ON by default, and that is the whole point: the failure it prevents (a proxy closing an
// idle stream at 60 seconds while the model is still thinking) does not look like a timeout
// to whoever hits it — it looks like the gateway dropping a good request. A feature that has
// to be discovered and enabled would leave the default deployment with the bug.
//
// Safe to default because a ping is an SSE COMMENT frame: the spec tells clients to ignore
// it, so no client has to know about this and none can misread it as content.
//
// 15 seconds sits under every idle timer that matters here (60s proxy default, 30s on an
// edge-optimized API Gateway) with room for one lost frame.
const defaultSSEKeepalive = 15 * time.Second

// effectiveKeepalive resolves the configured value.
//
// Zero means UNSET, not off — an int with omitempty cannot tell the two apart on the wire, so
// a NEGATIVE value is how a scope says "disable this". Reading zero as off would have made
// every deployment that never touched the field silently lose the protection.
func effectiveKeepalive(seconds int) time.Duration {
	switch {
	case seconds < 0:
		return 0 // explicitly disabled
	case seconds == 0:
		return defaultSSEKeepalive
	default:
		return time.Duration(seconds) * time.Second
	}
}

// streamDeadline derives the generation deadline for one request.
//
// The ceiling is the RUNTIME's own deadline, taken from ctx, minus the margin above — not a
// configured number. That is deliberate: the runtime already knows how long it may live, so
// reading it means the clamp cannot drift out of sync with the deployed Lambda timeout, and
// raising that timeout raises the ceiling automatically. A hardcoded ceiling would be one
// more value to keep aligned with Terraform, and the failure mode of getting it wrong is a
// stream cut with no final frame.
//
// A scope's request_timeout_ms only ever NARROWS: it is applied when it is shorter than what
// the runtime can honour, and ignored when it is longer. Same rule as the model ceiling.
// Zero means "no scope deadline", leaving the runtime's own as the only bound.
func streamDeadline(ctx context.Context, scopeMS int) (context.Context, context.CancelFunc) {
	ceiling := time.Duration(0)
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl) - streamDeadlineMargin; remaining > 0 {
			ceiling = remaining
		}
	}
	want := time.Duration(scopeMS) * time.Millisecond
	switch {
	case want <= 0 && ceiling <= 0:
		return context.WithCancel(ctx) // nothing to bound it with; caller still cancels
	case want <= 0:
		return context.WithTimeout(ctx, ceiling)
	case ceiling <= 0:
		return context.WithTimeout(ctx, want)
	case want < ceiling:
		return context.WithTimeout(ctx, want)
	default:
		return context.WithTimeout(ctx, ceiling)
	}
}

// chunkReader picks the richest read the stream offers.
//
// A stream implementing ports.ReasoningStream yields chain of thought as well as answer
// text; anything else is wrapped so the pump has one shape to consume. The type assertion
// is the same idiom the handler already uses for ports.StreamProvider itself.
func chunkReader(st ports.ProviderStream) func() (ports.Chunk, error) {
	if rs, ok := st.(ports.ReasoningStream); ok {
		return rs.RecvChunk
	}
	return func() (ports.Chunk, error) {
		t, err := st.Recv()
		return ports.Chunk{Text: t}, err
	}
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
