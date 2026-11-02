// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"errors"
	"testing"
)

// H1: a provider 4xx caused by the caller is classified as invalid_request (client error),
// not as a provider outage — so it does not become a 502 nor count against the SLI.
func TestClassifyProviderErr_ClientVsServer(t *testing.T) {
	client := []string{
		"bedrock_invoke: HTTP 400: {\"message\":\"Malformed input request: #/messages\"}",
		"operation error Bedrock Runtime: Converse, StatusCode: 400, ValidationException: prompt is too long: 200024 tokens > 200000 maximum",
		"HTTP 400: the provided temperature must be <= 1",
		"400 extraneous key [max_tokens] is not permitted",
		"temperature is deprecated for this model",
	}
	for _, m := range client {
		if got := classifyProviderErr(errors.New(m)); got != reasonInvalidRequest {
			t.Errorf("classifyProviderErr(%q) = %q, want %q", m, got, reasonInvalidRequest)
		}
	}
	// reasonInvalidRequest must be config/non-SLI so client mistakes do not burn the budget.
	if cat, sli := reasonMeta(reasonInvalidRequest); cat != catConfig || sli {
		t.Errorf("reasonMeta(invalid_request) = (%q,%v), want (config,false)", cat, sli)
	}
	// Genuine provider failures stay server-side (502, SLI-eligible).
	for _, m := range []string{"connection refused", "503 service unavailable", "i/o timeout", "throttling: 429"} {
		if got := classifyProviderErr(errors.New(m)); got == reasonInvalidRequest {
			t.Errorf("classifyProviderErr(%q) = invalid_request, must stay a provider failure", m)
		}
	}
}

func TestValidateRequest(t *testing.T) {
	ok := []chatMsg{{Role: "user", Content: []byte(`"hi"`)}}
	cases := []struct {
		name       string
		msgs       []chatMsg
		maxTok     *int
		temp, topP *float64
		wantReason bool
	}{
		{"valid", ok, ip(16), fp(0.5), fp(0.9), false},
		{"empty messages", nil, nil, nil, nil, true},
		{"temperature too high", ok, nil, fp(999), nil, true},
		{"temperature negative", ok, nil, fp(-1), nil, true},
		{"temperature 0 ok", ok, nil, fp(0), nil, false},
		{"top_p too high", ok, nil, nil, fp(5), true},
		{"top_p 0 ok", ok, nil, nil, fp(0), false},
		{"max_tokens zero", ok, ip(0), nil, nil, true},
		{"max_tokens negative", ok, ip(-5), nil, nil, true},
		{"max_tokens positive ok", ok, ip(16), nil, nil, false},
		{"missing role", []chatMsg{{Content: []byte(`"hi"`)}}, nil, nil, nil, true},
		{"invalid role", []chatMsg{{Role: "wizard", Content: []byte(`"hi"`)}}, nil, nil, nil, true},
	}
	for _, c := range cases {
		reason, detail := validateRequest(c.msgs, c.maxTok, c.temp, c.topP)
		got := reason != ""
		if got != c.wantReason {
			t.Errorf("%s: reason=%q detail=%q, wantReason=%v", c.name, reason, detail, c.wantReason)
		}
		if got && reason != reasonInvalidRequest {
			t.Errorf("%s: reason=%q, want invalid_request", c.name, reason)
		}
	}
}
