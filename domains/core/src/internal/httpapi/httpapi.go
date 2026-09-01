// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package httpapi is the Core's INBOUND PORT, neutral with respect to runtime.
//
// Feature: hexagonal-refactor, Requirements 1.1, 1.2.
//
// Request and Response carry NO Lambda type. That is what lets the Core be served
// either by the Lambda adapter (internal/awslambda) or by a local
// http.ListenAndServe or, in the future, by Fargate/App Runner — without rewriting
// the decision logic. The body arrives already decoded (no IsBase64Encoded): that
// decision belongs to the inbound adapter, not to this layer.
package httpapi

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// Request is the inbound boundary. Body is always already decoded text.
type Request struct {
	Method    string
	Path      string
	Headers   map[string]string
	Body      string
	RequestID string
}

// Response is the outbound boundary.
type Response struct {
	StatusCode int
	Headers    map[string]string
	Body       string
}

// HandlerFunc is the type of the Core's decision function. It depends on neither
// http.Handler nor Lambda types — it is what both adapters (awslambda and this
// package) converge on calling.
type HandlerFunc func(ctx context.Context, req Request) (Response, error)

// New converts a HandlerFunc into a standard library http.Handler.
//
// This is what satisfies R1.1 and R1.5: the Core can be exposed as an ordinary HTTP
// server, with no Lambda SDK anywhere along the path.
func New(h HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := fromHTTPRequest(r)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		resp, err := h(r.Context(), req)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeHTTPResponse(w, resp)
	})
}

// fromHTTPRequest translates *http.Request into the neutral boundary. The body is
// read whole (gateway requests are small: only chat JSON, never an upload);
// multi-value headers collapse to the first value, the same convention the Lambda
// adapter already applied by hand.
func fromHTTPRequest(r *http.Request) (Request, error) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return Request{}, err
	}
	// Lowercase keys: net/http canonicalizes to "Authorization", but API Gateway v2
	// delivers "authorization". Normalizing here makes both inbound paths equivalent
	// (R1.6) and removes the trap of a reader using req.Headers["authorization"]
	// directly and only working through Lambda.
	headers := make(map[string]string, len(r.Header))
	for k, vs := range r.Header {
		if len(vs) > 0 {
			headers[strings.ToLower(k)] = vs[0]
		}
	}
	reqID := r.Header.Get("x-amzn-requestid")
	if reqID == "" {
		reqID = r.Header.Get("x-request-id")
	}
	return Request{
		Method:    r.Method,
		Path:      r.URL.Path,
		Headers:   headers,
		Body:      string(b),
		RequestID: reqID,
	}, nil
}

func writeHTTPResponse(w http.ResponseWriter, resp Response) {
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	if resp.StatusCode == 0 {
		resp.StatusCode = http.StatusOK
	}
	w.WriteHeader(resp.StatusCode)
	if resp.Body != "" {
		io.WriteString(w, resp.Body)
	}
}
