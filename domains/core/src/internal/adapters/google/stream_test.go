// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package google

// Two Gemini-specific traps are pinned here, both of which produce a plausible answer and
// a wrong number:
//   · usageMetadata is CUMULATIVE, repeated on every chunk. Adding it up multiplies the
//     token count (and the cost) by roughly the number of chunks.
//   · the URL needs alt=sse. Without it :streamGenerateContent returns a chunked JSON
//     array, an SSE reader finds no `data:` lines, and the request finishes with an empty
//     answer and no error.

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

// Cumulative usage: the counts grow chunk by chunk and the LAST value is the total.
const geminiStreamBody = `data: {"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"}}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":1,"cachedContentTokenCount":900}}

data: {"candidates":[{"content":{"parts":[{"text":", world"}],"role":"model"}}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":5,"cachedContentTokenCount":900}}

data: {"candidates":[{"content":{"parts":[{"text":"!"},{"text":" Bye"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":9,"cachedContentTokenCount":900}}

`

func TestOpenStreamCumulativeUsageIsNotSummed(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, geminiStreamBody)
	}))
	defer srv.Close()

	a := &Adapter{HTTP: srv.Client(), BaseURL: srv.URL, ModelID: "gemini-x", APIKey: "k"}
	st, err := a.OpenStream(context.Background(), ports.InvokeInput{Messages: []ports.Message{{Role: "user", Text: "hi"}}})
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	deltas, err := drainAll(t, st)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("final error = %v, expected io.EOF", err)
	}

	// Three chunks, and the third carries TWO parts joined in order — taking parts[0]
	// only, as the buffered path does, would drop " Bye".
	if len(deltas) != 3 || deltas[2] != "! Bye" {
		t.Errorf("deltas = %q, expected the third to be \"! Bye\"", deltas)
	}
	res := st.Result()
	if res.Text != "Hello, world! Bye" {
		t.Errorf("Text = %q", res.Text)
	}
	// 1000, not 3000. Summing three cumulative blocks is the bug.
	if res.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, expected 1000 (cumulative, assigned not summed)", res.InputTokens)
	}
	if res.OutputTokens != 9 {
		t.Errorf("OutputTokens = %d, expected the last cumulative value 9", res.OutputTokens)
	}
	if res.CacheReadInputTokens != 900 || res.CacheCounters != ports.CacheCountersReported {
		t.Errorf("cache = %d/%q, expected 900/reported", res.CacheReadInputTokens, res.CacheCounters)
	}
	if res.StopReason != "STOP" {
		t.Errorf("StopReason = %q, expected STOP", res.StopReason)
	}
	// alt=sse and the streaming method, or the body is not SSE at all.
	if !strings.Contains(gotURL, "streamGenerateContent") || !strings.Contains(gotURL, "alt=sse") {
		t.Errorf("URL = %q, expected :streamGenerateContent with alt=sse", gotURL)
	}
	if err := st.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// No cachedContentTokenCount must read as ABSENT rather than a reported zero.
func TestOpenStreamNoCacheReportsAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":2}}\n\n")
	}))
	defer srv.Close()
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

func TestOpenStreamErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"bad model"}}`)
	}))
	defer srv.Close()
	a := &Adapter{HTTP: srv.Client(), BaseURL: srv.URL, ModelID: "m", APIKey: "k"}
	st, err := a.OpenStream(context.Background(), ports.InvokeInput{Messages: []ports.Message{{Role: "user", Text: "hi"}}})
	if err == nil {
		t.Fatal("expected an error for a 400")
	}
	if st != nil {
		t.Error("a failed open must not return a stream")
	}
	if !strings.Contains(err.Error(), "bad model") {
		t.Errorf("error should carry the provider text: %v", err)
	}
}

// The two paths differ ONLY in the endpoint: same body, so a streaming request cannot
// reach a differently-shaped prompt than a buffered one.
func TestBufferedAndStreamShareTheBody(t *testing.T) {
	in := ports.InvokeInput{Messages: []ports.Message{
		{Role: "system", Text: "be brief"},
		{Role: "user", Text: "hi"},
	}}
	a := &Adapter{HTTP: http.DefaultClient, BaseURL: "http://x", ModelID: "m", APIKey: "k"}
	read := func(stream bool) (string, string) {
		req, err := a.buildRequest(context.Background(), in, stream)
		if err != nil {
			t.Fatalf("buildRequest(%v): %v", stream, err)
		}
		b, _ := io.ReadAll(req.Body)
		return req.URL.String(), string(b)
	}
	bufURL, bufBody := read(false)
	strURL, strBody := read(true)
	if bufBody != strBody {
		t.Errorf("bodies differ:\n buffered=%s\n streamed=%s", bufBody, strBody)
	}
	if !strings.Contains(bufURL, ":generateContent") || strings.Contains(bufURL, "alt=sse") {
		t.Errorf("buffered URL = %q", bufURL)
	}
	if !strings.Contains(strURL, ":streamGenerateContent") || !strings.Contains(strURL, "alt=sse") {
		t.Errorf("streaming URL = %q", strURL)
	}
	if !strings.Contains(bufBody, "systemInstruction") {
		t.Errorf("system instruction missing: %s", bufBody)
	}
}
