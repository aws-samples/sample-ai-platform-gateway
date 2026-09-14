// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Wire-format tests for the streaming adapter.
//
// What is under test is OUR wiring, not the SDK: that a complete body and an
// incremental one both come out in the format API Gateway requires, that the pipe is
// always closed (an unclosed writer hangs the invocation until the deadline, the worst
// failure mode available here), and that a panic in the producer goroutine surfaces as
// a stream error instead of taking the process down.
package awslambda

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aiplat/core/internal/httpapi"
)

// delimiter is the 8-null-byte separator between the metadata JSON and the payload.
var delimiter = []byte{0, 0, 0, 0, 0, 0, 0, 0}

// splitPrelude returns (metadata JSON, payload) from a streamed response body.
func splitPrelude(t *testing.T, r io.Reader) (string, string) {
	t.Helper()
	all, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	i := bytes.Index(all, delimiter)
	if i < 0 {
		t.Fatalf("no 8-null-byte delimiter in output: %q", all)
	}
	if i > 16*1024 {
		t.Errorf("delimiter at byte %d, must be within the first 16KB", i)
	}
	return string(all[:i]), string(all[i+len(delimiter):])
}

// A complete body is a stream of one chunk. This is the path EVERY non-streaming
// response takes (401, 400, plain JSON completions) once the integration is STREAM:
// they must still carry the prelude or API Gateway answers 500.
func TestFromResponseStream_CompleteBody(t *testing.T) {
	out := FromResponseStream(httpapi.Response{
		StatusCode: 201,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       `{"ok":true}`,
	})

	meta, payload := splitPrelude(t, out)
	if !strings.Contains(meta, `"statusCode":201`) {
		t.Errorf("metadata missing statusCode: %s", meta)
	}
	if !strings.Contains(meta, `"content-type":"application/json"`) {
		t.Errorf("metadata missing header: %s", meta)
	}
	if payload != `{"ok":true}` {
		t.Errorf("payload = %q", payload)
	}
}

// An absent status must not reach API Gateway as 0.
func TestFromResponseStream_DefaultsStatus(t *testing.T) {
	out := FromResponseStream(httpapi.Response{Body: "x"})
	meta, _ := splitPrelude(t, out)
	if !strings.Contains(meta, `"statusCode":200`) {
		t.Errorf("a missing StatusCode should default to 200, got: %s", meta)
	}
}

// The incremental path: whatever the producer writes becomes the payload, and the
// reader sees EOF once the producer returns (i.e. the pipe was closed).
func TestFromResponseStream_Incremental(t *testing.T) {
	out := FromResponseStream(httpapi.Response{
		StatusCode: 200,
		Headers:    map[string]string{"content-type": "text/event-stream"},
		Stream: func(w io.Writer) {
			io.WriteString(w, "data: a\n\n")
			io.WriteString(w, "data: b\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
		},
	})

	meta, payload := splitPrelude(t, out)
	if !strings.Contains(meta, `"content-type":"text/event-stream"`) {
		t.Errorf("metadata missing SSE content-type: %s", meta)
	}
	if payload != "data: a\n\ndata: b\n\ndata: [DONE]\n\n" {
		t.Errorf("payload = %q", payload)
	}
}

// A producer that writes nothing must still terminate the stream. If the pipe were left
// open the runtime would block reading until the invocation deadline.
func TestFromResponseStream_EmptyProducerTerminates(t *testing.T) {
	out := FromResponseStream(httpapi.Response{StatusCode: 204, Stream: func(w io.Writer) {}})
	_, payload := splitPrelude(t, out)
	if payload != "" {
		t.Errorf("payload = %q, expected empty", payload)
	}
}

// A panic in the producer must become a READ ERROR, so the invocation fails loudly.
// Without the recover it would crash the process, because the goroutine is not on the
// handler's stack and the SDK's own recover never sees it.
func TestFromResponseStream_PanicBecomesStreamError(t *testing.T) {
	out := FromResponseStream(httpapi.Response{
		StatusCode: 200,
		Stream: func(w io.Writer) {
			io.WriteString(w, "partial")
			panic("boom")
		},
	})

	_, err := io.ReadAll(out)
	if err == nil {
		t.Fatal("expected a read error after the producer panicked")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("the error should carry the panic value, got: %v", err)
	}
}

// AdaptStream must preserve the inbound translation Adapt already does (base64 body,
// lowercase header access) and hand back the streaming type.
func TestAdaptStream_PassesRequestThrough(t *testing.T) {
	var gotBody, gotAuth string
	h := func(_ context.Context, req httpapi.Request) (httpapi.Response, error) {
		gotBody = req.Body
		gotAuth = req.Headers["authorization"]
		return httpapi.Response{StatusCode: 200, Body: "done"}, nil
	}

	fn := AdaptStream(h)
	out, err := fn(context.Background(), events.APIGatewayProxyRequest{
		HTTPMethod:      "POST",
		Path:            "/v1/chat/completions",
		Headers:         map[string]string{"authorization": "Bearer k"},
		Body:            "eyJhIjoxfQ==", // {"a":1}
		IsBase64Encoded: true,
	})
	if err != nil {
		t.Fatalf("AdaptStream: %v", err)
	}
	if gotBody != `{"a":1}` {
		t.Errorf("body not base64-decoded: %q", gotBody)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("authorization = %q", gotAuth)
	}
	_, payload := splitPrelude(t, out)
	if payload != "done" {
		t.Errorf("payload = %q", payload)
	}
}
