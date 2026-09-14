// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package awslambda is the ONLY place in the Core that knows about the API
// Gateway event type. Pure protocol translation — no business decision here
// (Requirement 1.4).
package awslambda

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aiplat/core/internal/httpapi"
)

// Adapt wraps an httpapi.HandlerFunc into the shape lambda.Start expects.
//
// REST API event (APIGatewayProxyRequest/Response), not the HTTP API's v2 shape —
// the router moved here for AWS WAF, resource policies and private-endpoint support,
// none of which HTTP API offers. Auth for this Lambda stays exactly as it was: an
// API key checked inside the domain (authResolve in internal/gateway), never
// Cognito — REST API's COGNITO_USER_POOLS authorizer is irrelevant here and is not
// attached to this route.
func Adapt(h httpapi.HandlerFunc) func(context.Context, events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	return func(ctx context.Context, e events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
		req, err := ToRequest(e)
		if err != nil {
			return events.APIGatewayProxyResponse{StatusCode: 400, Body: "bad request"}, nil
		}
		resp, err := h(ctx, req)
		if err != nil {
			return events.APIGatewayProxyResponse{}, err
		}
		return FromResponse(resp), nil
	}
}

// ToRequest translates the API Gateway event into the Core's neutral boundary.
//
// Preserves: method, path, headers and body (decoding base64 when API Gateway
// signals IsBase64Encoded — this decoding used to live inside handle(); moving it
// here is what frees the domain from knowing base64 sits along the way).
func ToRequest(e events.APIGatewayProxyRequest) (httpapi.Request, error) {
	raw := e.Body
	if e.IsBase64Encoded {
		if dec, derr := base64.StdEncoding.DecodeString(raw); derr == nil {
			raw = string(dec)
		}
	}
	path := e.Path
	if path == "" {
		path = e.Resource
	}
	return httpapi.Request{
		Method:    e.HTTPMethod,
		Path:      path,
		Headers:   e.Headers,
		Body:      raw,
		RequestID: e.RequestContext.RequestID,
	}, nil
}

// FromResponse translates the neutral response back into the API Gateway format.
//
// An INCREMENTAL response is materialized here: the producer is drained into a buffer
// and sent as one body. That is the honest behaviour for a buffered integration — the
// frames are valid SSE, they just all arrive at the end. Without this, a `stream: true`
// request through the buffered path would return an empty body, because Body is unset
// whenever Stream is set.
func FromResponse(r httpapi.Response) events.APIGatewayProxyResponse {
	body := r.Body
	if r.Stream != nil {
		var buf bytes.Buffer
		r.Stream(&buf)
		body = buf.String()
	}
	return events.APIGatewayProxyResponse{
		StatusCode: r.StatusCode,
		Headers:    r.Headers,
		Body:       body,
	}
}

// --- Response streaming ------------------------------------------------------
//
// AdaptStream differs from Adapt in the RETURN TYPE only:
// *events.APIGatewayProxyStreamingResponse instead of the buffered
// APIGatewayProxyResponse. The inbound translation is identical — API Gateway sends the
// same event either way.
//
// The return type alone does NOT produce a streamed response: aws-lambda-go never sets
// `Lambda-Runtime-Function-Response-Mode: streaming` on its response POST, so under
// lambda.Start the bytes reach Lambda buffered no matter which type the handler returns.
// That header is why cmd/router uses StartStreaming (internal/awslambda/runtimeapi.go)
// rather than lambda.Start(AdaptStream(...)) when streaming is enabled.
//
// AdaptStream is kept because it is the lambda.Start-compatible shape, and it becomes the
// one-line wiring the moment aws-lambda-go implements the header upstream — at which point
// runtimeapi.go can be deleted.

// AdaptStream wraps an httpapi.HandlerFunc into the shape lambda.Start expects for a
// streaming integration.
func AdaptStream(h httpapi.HandlerFunc) func(context.Context, events.APIGatewayProxyRequest) (*events.APIGatewayProxyStreamingResponse, error) {
	return func(ctx context.Context, e events.APIGatewayProxyRequest) (*events.APIGatewayProxyStreamingResponse, error) {
		req, err := ToRequest(e)
		if err != nil {
			return FromResponseStream(httpapi.Response{StatusCode: 400, Body: "bad request"}), nil
		}
		resp, err := h(ctx, req)
		if err != nil {
			return nil, err
		}
		return FromResponseStream(resp), nil
	}
}

// FromResponseStream translates the neutral response into the streaming format: the
// metadata JSON, the 8-null-byte delimiter and then the payload. The SDK's response type
// writes that wire format; nothing here does it by hand.
//
// For a complete body it is a reader over the bytes. For an incremental one it is an
// io.Pipe fed by a goroutine, so frames would leave the function as they are produced.
// The invocation context stays valid for that window — aws-lambda-go cancels it only
// after the response body has been read (handleInvoke defers cancel past
// invoke.success) — which is what makes it safe for the producer to keep using ctx
// after this function returns.
func FromResponseStream(r httpapi.Response) *events.APIGatewayProxyStreamingResponse {
	status := r.StatusCode
	if status == 0 {
		status = 200
	}
	out := &events.APIGatewayProxyStreamingResponse{StatusCode: status, Headers: r.Headers}

	if r.Stream == nil {
		out.Body = strings.NewReader(r.Body)
		return out
	}

	pr, pw := io.Pipe()
	go func() {
		// The pipe MUST be closed on every path. An unclosed writer leaves the runtime
		// blocked reading until the invocation deadline — a hang, not an error, which is
		// the worst failure mode available here. A panic in this goroutine would also
		// take the whole process down (it is not on the handler's stack, so the SDK's
		// recover never sees it), so it is converted into a stream error: the reader
		// fails, the invocation fails loudly, and the log carries the panic value.
		defer func() {
			if p := recover(); p != nil {
				pw.CloseWithError(fmt.Errorf("panic while streaming response: %v", p))
				return
			}
			pw.Close()
		}()
		r.Stream(pw)
	}()
	out.Body = pr
	return out
}
