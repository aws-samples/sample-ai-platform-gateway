// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Equivalence test for the two inbound paths (hexagonal-refactor, R1.6).
//
// The same logical request, served by the direct http.Handler (httpapi.New) and by
// the Lambda adapter (awslambda.Adapt), must reach the HandlerFunc identically and
// return an identical response. That is what guarantees the inbound adapter does not
// change semantics — without it, migrating to Fargate/a local server would change
// behavior silently.
//
// The handler used is an ECHO: it does not touch AWS. The goal is to exercise the
// protocol translation on both sides, not the Core's decision logic.
package awslambda

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aiplat/core/internal/httpapi"
)

// echo returns what it received, so we can compare what each path delivered.
func echo(_ context.Context, req httpapi.Request) (httpapi.Response, error) {
	b, _ := json.Marshal(map[string]string{
		"method": req.Method,
		"path":   req.Path,
		"body":   req.Body,
		"auth":   req.Headers["authorization"],
	})
	return httpapi.Response{
		StatusCode: 200,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       string(b),
	}, nil
}

func TestEntradasEquivalentes(t *testing.T) {
	const (
		method = "POST"
		path   = "/v1/chat/completions"
		body   = `{"model":"claude-sonnet","messages":[{"role":"user","content":"oi"}]}`
		auth   = "Bearer sk-aiplat-teste"
	)

	// Path 1: direct http.Handler (local server / Fargate).
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(method, path, strings.NewReader(body))
	httpReq.Header.Set("authorization", auth)
	httpapi.New(echo).ServeHTTP(rec, httpReq)
	direto := rec.Result()

	// Path 2: Lambda adapter (API Gateway REST API — APIGatewayProxyRequest, the
	// event shape used since the router moved off HTTP API for WAF support).
	ev := events.APIGatewayProxyRequest{
		Path:       path,
		HTTPMethod: method,
		Headers:    map[string]string{"authorization": auth},
		Body:       body,
	}
	viaLambda, err := Adapt(echo)(context.Background(), ev)
	if err != nil {
		t.Fatalf("Lambda adapter: %v", err)
	}

	// Same status.
	if direto.StatusCode != viaLambda.StatusCode {
		t.Errorf("status: direct=%d lambda=%d", direto.StatusCode, viaLambda.StatusCode)
	}

	// Same body → the HandlerFunc received the SAME Request through both paths.
	var corpoDireto map[string]string
	json.NewDecoder(direto.Body).Decode(&corpoDireto)
	var corpoLambda map[string]string
	json.Unmarshal([]byte(viaLambda.Body), &corpoLambda)

	for _, k := range []string{"method", "path", "body", "auth"} {
		if corpoDireto[k] != corpoLambda[k] {
			t.Errorf("field %q diverged: direct=%q lambda=%q", k, corpoDireto[k], corpoLambda[k])
		}
	}
	if corpoLambda["method"] != method || corpoLambda["path"] != path ||
		corpoLambda["body"] != body || corpoLambda["auth"] != auth {
		t.Errorf("the Request was not preserved: %+v", corpoLambda)
	}
}

// A base64 body (API Gateway sometimes sets IsBase64Encoded) is decoded by the
// adapter, so the domain never sees base64 (R1.3).
func TestBase64Decodificado(t *testing.T) {
	const body = `{"model":"x"}`
	ev := events.APIGatewayProxyRequest{
		Path:            "/v1/chat/completions",
		HTTPMethod:      "POST",
		Body:            "eyJtb2RlbCI6IngifQ==", // base64 of {"model":"x"}
		IsBase64Encoded: true,
	}

	req, err := ToRequest(ev)
	if err != nil {
		t.Fatalf("ToRequest: %v", err)
	}
	if req.Body != body {
		t.Errorf("body = %q, expected %q (base64 not decoded)", req.Body, body)
	}
}
