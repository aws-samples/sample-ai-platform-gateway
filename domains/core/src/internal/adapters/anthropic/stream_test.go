// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package anthropic

// What these pin is the token accounting, not the text. A streaming answer that reports
// the wrong usage still reads perfectly on screen — the only place it shows up is the
// bill, later, with no error to trace back to.
//
// Anthropic splits usage across two events, and each half has a trap: message_start
// carries `output_tokens: 1` as a placeholder (taking it makes every response cost one
// token) and carries the prompt-cache counters (skipping it loses the cache read).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aiplat/core/internal/ports"
)

// sseServer replies with a fixed SSE body and records the request it received.
func sseServer(t *testing.T, body string) (*httptest.Server, *http.Request, *[]byte) {
	t.Helper()
	var got *http.Request
	var payload []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		payload, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, got, &payload
}

func drainAll(t *testing.T, st ports.ProviderStream) ([]string, error) {
	t.Helper()
	var out []string
	for i := 0; i < 200; i++ {
		txt, err := st.Recv()
		if txt != "" {
			out = append(out, txt)
		}
		if err != nil {
			return out, err
		}
	}
	t.Fatal("Recv did not terminate")
	return nil, nil
}

const anthropicStreamBody = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":900,"cache_creation_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", world"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

func TestOpenStreamParsesEventsAndUsage(t *testing.T) {
	srv, _, payload := sseServer(t, anthropicStreamBody)
	a := &Adapter{HTTP: srv.Client(), BaseURL: srv.URL, ModelID: "claude-x", APIKey: "k"}

	st, err := a.OpenStream(context.Background(), ports.InvokeInput{
		Messages: []ports.Message{{Role: "user", Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	deltas, err := drainAll(t, st)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("final error = %v, expected io.EOF", err)
	}

	// One frame per text_delta, and NOTHING for ping / content_block_start /
	// input_json_delta — tool arguments are not visible output and must not be emitted as
	// if they were prose.
	if len(deltas) != 2 || deltas[0] != "Hello" || deltas[1] != ", world" {
		t.Errorf("deltas = %q, expected [Hello ,ecomma world]", deltas)
	}

	res := st.Result()
	if res.Text != "Hello, world" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.InputTokens != 100 {
		t.Errorf("InputTokens = %d, expected 100 from message_start", res.InputTokens)
	}
	// The whole point: 42 from message_delta, NOT the placeholder 1 from message_start.
	if res.OutputTokens != 42 {
		t.Errorf("OutputTokens = %d, expected 42 from message_delta (1 is message_start's placeholder)", res.OutputTokens)
	}
	if res.CacheReadInputTokens != 900 || res.CacheCounters != ports.CacheCountersReported {
		t.Errorf("cache read = %d counters = %q, expected 900/reported from message_start",
			res.CacheReadInputTokens, res.CacheCounters)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", res.StopReason)
	}
	// The request must ask for a stream, or the provider answers with one JSON body and
	// the SSE reader finds nothing.
	if !strings.Contains(string(*payload), `"stream":true`) {
		t.Errorf("request body does not set stream:true: %s", string(*payload))
	}
	if err := st.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// No cache counters reported must read as ABSENT, not as zero: Anthropic's input_tokens
// excludes cached tokens, so "did not say" and "said zero" price differently.
func TestOpenStreamAbsentCacheCounters(t *testing.T) {
	srv, _, _ := sseServer(t, `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

`)
	a := &Adapter{HTTP: srv.Client(), BaseURL: srv.URL, ModelID: "m", APIKey: "k"}
	st, err := a.OpenStream(context.Background(), ports.InvokeInput{Messages: []ports.Message{{Role: "user", Text: "hi"}}})
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	drainAll(t, st)
	if got := st.Result().CacheCounters; got != ports.CacheCountersAbsent {
		t.Errorf("CacheCounters = %q, expected absent", got)
	}
}

// A non-2xx must NOT become a stream: the body is an error message, and wrapping it would
// deliver the provider's error text to the client as if it were the model's answer. It
// also has to stay an error so the gateway can try the next route.
func TestOpenStreamErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer srv.Close()
	a := &Adapter{HTTP: srv.Client(), BaseURL: srv.URL, ModelID: "m", APIKey: "k"}
	st, err := a.OpenStream(context.Background(), ports.InvokeInput{Messages: []ports.Message{{Role: "user", Text: "hi"}}})
	if err == nil {
		t.Fatal("expected an error for a 429")
	}
	if st != nil {
		t.Error("a failed open must not return a stream")
	}
	if !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error should carry the status and the provider text: %v", err)
	}
}

// The buffered and streaming paths must send the same body apart from `stream`. A
// difference in the prompt-caching block would change cost without changing behaviour,
// which is the hardest kind of drift to notice.
func TestBufferedAndStreamBodiesMatchApartFromStream(t *testing.T) {
	in := ports.InvokeInput{Messages: []ports.Message{
		{Role: "system", Text: "be brief"},
		{Role: "user", Text: "hi"},
	}}
	a := &Adapter{HTTP: http.DefaultClient, BaseURL: "http://x", ModelID: "m", APIKey: "k", CachePrefix: true}

	read := func(stream bool) string {
		req, err := a.buildRequest(context.Background(), in, stream)
		if err != nil {
			t.Fatalf("buildRequest(%v): %v", stream, err)
		}
		b, _ := io.ReadAll(req.Body)
		return string(b)
	}
	buffered, streamed := read(false), read(true)
	if strings.Contains(buffered, `"stream"`) {
		t.Errorf("the buffered body must not set stream: %s", buffered)
	}
	if !strings.Contains(streamed, `"stream":true`) {
		t.Errorf("the streaming body must set stream:true: %s", streamed)
	}
	// Both must carry the cache_control block, which is the field a divergence would hide.
	for name, body := range map[string]string{"buffered": buffered, "streamed": streamed} {
		if !strings.Contains(body, `"cache_control":{"type":"ephemeral"}`) {
			t.Errorf("%s body lost the prompt-cache block: %s", name, body)
		}
	}
}
