// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package bedrockinvoke

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/aiplat/core/internal/ports"
)

// eventFrame encodes one inner Converse chunk as a Bedrock event-stream "event" message.
// The inner JSON is wrapped base64 inside {"bytes":...} exactly as the live gateway sends
// it (encoding/json renders a []byte field as base64, which is the wire shape).
func eventFrame(t *testing.T, enc *eventstream.Encoder, w io.Writer, innerJSON string) {
	t.Helper()
	payload, err := json.Marshal(struct {
		Bytes []byte `json:"bytes"`
	}{Bytes: []byte(innerJSON)})
	if err != nil {
		t.Fatal(err)
	}
	msg := eventstream.Message{
		Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("event")},
			{Name: ":event-type", Value: eventstream.StringValue("chunk")},
			{Name: ":content-type", Value: eventstream.StringValue("application/json")},
		},
		Payload: payload,
	}
	if err := enc.Encode(w, msg); err != nil {
		t.Fatal(err)
	}
}

// exceptionFrame encodes a mid-stream exception message, which the adapter must surface as
// an error rather than silently ending the stream.
func exceptionFrame(t *testing.T, enc *eventstream.Encoder, w io.Writer, body string) {
	t.Helper()
	msg := eventstream.Message{
		Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("exception")},
			{Name: ":exception-type", Value: eventstream.StringValue("ThrottlingException")},
			{Name: ":content-type", Value: eventstream.StringValue("application/json")},
		},
		Payload: []byte(body),
	}
	if err := enc.Encode(w, msg); err != nil {
		t.Fatal(err)
	}
}

// novaStreamBody builds the full native event-stream a Nova InvokeModelWithResponseStream
// call returns: a messageStart, text deltas, contentBlockStop, messageStop, and a trailing
// metadata chunk carrying usage. Shape measured from the live gateway on 2026-10-26.
func novaStreamBody(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := eventstream.NewEncoder()
	eventFrame(t, enc, &buf, `{"messageStart":{"role":"assistant"}}`)
	eventFrame(t, enc, &buf, `{"contentBlockDelta":{"delta":{"text":"Hel"},"contentBlockIndex":0}}`)
	eventFrame(t, enc, &buf, `{"contentBlockDelta":{"delta":{"text":"lo"},"contentBlockIndex":0}}`)
	eventFrame(t, enc, &buf, `{"contentBlockStop":{"contentBlockIndex":0}}`)
	eventFrame(t, enc, &buf, `{"messageStop":{"stopReason":"end_turn"}}`)
	eventFrame(t, enc, &buf, `{"metadata":{"usage":{"inputTokens":7,"outputTokens":2,`+
		`"cacheReadInputTokenCount":5,"cacheWriteInputTokenCount":1}}}`)
	return buf.Bytes()
}

func newStreamResp(status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}
}

func drain(t *testing.T, s ports.ProviderStream) string {
	t.Helper()
	var got strings.Builder
	for {
		delta, err := s.Recv()
		got.WriteString(delta)
		if err == io.EOF {
			return got.String()
		}
		if err != nil {
			t.Fatalf("unexpected stream error: %v", err)
		}
	}
}

func TestOpenStream_DeltasAndTrailingUsage(t *testing.T) {
	GW := "https://gw-abc.gateway.bedrock-agentcore.us-east-1.amazonaws.com"
	sg := &fakeSigner{}
	var gotURL, gotAccept string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		gotAccept = r.Header.Get("accept")
		return newStreamResp(200, novaStreamBody(t)), nil
	})
	p, err := New(&http.Client{Transport: rt}, sg, Route{BaseURL: GW, ModelID: "us.amazon.nova-micro-v1:0"})
	if err != nil {
		t.Fatal(err)
	}
	sp, ok := p.(ports.StreamProvider)
	if !ok {
		t.Fatal("adapter must implement ports.StreamProvider")
	}
	st, err := sp.OpenStream(context.Background(), ports.InvokeInput{
		Messages: []ports.Message{{Role: "user", Text: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// The ':' id must survive in the streaming path, and the request must negotiate the
	// binary event-stream.
	if !strings.Contains(gotURL, "/inference/model/us.amazon.nova-micro-v1:0/invoke-with-response-stream") {
		t.Fatalf("stream path wrong: %s", gotURL)
	}
	if gotAccept != "application/vnd.amazon.eventstream" {
		t.Fatalf("accept header wrong: %q", gotAccept)
	}
	if !sg.called || sg.service != "bedrock-agentcore" || sg.region != "us-east-1" {
		t.Fatalf("stream request not signed correctly: %+v", sg)
	}

	if text := drain(t, st); text != "Hello" {
		t.Fatalf("assembled text wrong: %q", text)
	}
	// Usage is a trailing chunk: only readable once the stream has ended.
	res := st.Result()
	if res.Text != "Hello" || res.StopReason != "end_turn" {
		t.Fatalf("result text/stop wrong: %+v", res)
	}
	if res.InputTokens != 7 || res.OutputTokens != 2 {
		t.Fatalf("usage tokens wrong: %+v", res)
	}
	if res.CacheReadInputTokens != 5 || res.CacheWriteInputTokens != 1 {
		t.Fatalf("cache counters wrong: %+v", res)
	}
	if res.CacheCounters != ports.CacheCountersReported {
		t.Fatalf("usage present must be Reported, got %q", res.CacheCounters)
	}
}

func TestOpenStream_ExceptionMidStreamSurfaces(t *testing.T) {
	GW := "https://gw-abc.gateway.bedrock-agentcore.us-east-1.amazonaws.com"
	var buf bytes.Buffer
	enc := eventstream.NewEncoder()
	eventFrame(t, enc, &buf, `{"contentBlockDelta":{"delta":{"text":"partial"}}}`)
	exceptionFrame(t, enc, &buf, `{"message":"rate exceeded"}`)
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return newStreamResp(200, buf.Bytes()), nil
	})
	p, _ := New(&http.Client{Transport: rt}, &fakeSigner{}, Route{BaseURL: GW, ModelID: "us.amazon.nova-micro-v1:0"})
	st, err := p.(ports.StreamProvider).OpenStream(context.Background(), ports.InvokeInput{
		Messages: []ports.Message{{Role: "user", Text: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// First delta arrives; the partial answer must be kept, not discarded.
	delta, err := st.Recv()
	if err != nil || delta != "partial" {
		t.Fatalf("first delta wrong: %q err=%v", delta, err)
	}
	// The exception must become a non-EOF error so the caller stops and bills what it got.
	_, err = st.Recv()
	if err == nil || err == io.EOF {
		t.Fatalf("exception must surface as a non-EOF error, got %v", err)
	}
	if !strings.Contains(err.Error(), "exception") {
		t.Fatalf("error should name the exception: %v", err)
	}
	if st.Result().Text != "partial" {
		t.Fatalf("partial text must be retained, got %q", st.Result().Text)
	}
}

func TestOpenStream_HTTPErrorFallsBack(t *testing.T) {
	GW := "https://gw-abc.gateway.bedrock-agentcore.us-east-1.amazonaws.com"
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return newResp(429, `{"message":"Too Many Requests"}`), nil
	})
	p, _ := New(&http.Client{Transport: rt}, &fakeSigner{}, Route{BaseURL: GW, ModelID: "us.amazon.nova-micro-v1:0"})
	// OpenStream must return (nil, error) — never a stream alongside an error — so the
	// handler can still fall back before committing the client's HTTP status.
	st, err := p.(ports.StreamProvider).OpenStream(context.Background(), ports.InvokeInput{
		Messages: []ports.Message{{Role: "user", Text: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want error carrying status 429, got %v", err)
	}
	if st != nil {
		t.Fatalf("no stream may be returned with an error, got %#v", st)
	}
}
