// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"errors"
	"testing"

	"github.com/aiplat/core/internal/ports"
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

// CORE-BND-002: classification prefers the typed status code carried by
// ports.ProviderError over grepping the message text. The adapters format their errors
// as "<label> <status>: <body>" (e.g. "anthropic 400: {...}"), which contains none of
// the "http 400"/"statuscode: 400" tokens the string matcher keys on — so before this
// fix a provider 400 fell through to "provider_error" (502 + SLI). With the typed error
// it classifies on the number.
func TestClassifyProviderErr_TypedStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"type":"error","error":{"type":"invalid_request_error"}}`, reasonInvalidRequest},
		{404, "model not found", reasonProviderConfig}, // CORE-BND-006: a 404 is a route mistake, not a caller one
		{422, "unprocessable", reasonInvalidRequest},
		{401, "unauthorized", "provider_auth"},
		{403, "forbidden", "provider_auth"},
		{429, "slow down", "provider_rate_limited"},
		{402, "insufficient credit", "provider_quota_exceeded"},
		{500, "internal", "provider_down"},
		{503, "unavailable", "provider_down"},
	}
	for _, c := range cases {
		err := &ports.ProviderError{Status: c.status, Label: "anthropic", Body: c.body}
		if got := classifyProviderErr(err); got != c.want {
			t.Errorf("status %d: classifyProviderErr = %q, want %q", c.status, got, c.want)
		}
	}
	// no_eligible_model is a config refusal, never an SLI-eligible dependency failure.
	if cat, sli := reasonMeta("no_eligible_model"); cat != catConfig || sli {
		t.Errorf("reasonMeta(no_eligible_model) = (%q,%v), want (config,false)", cat, sli)
	}
}

// CORE-BND-006: a provider rejection that the MODEL ID cannot be resolved (an operator
// route mistake) is classified as provider_config (config, non-SLI, route-oriented
// message), NOT the caller-blaming invalid_request. A genuine parameter rejection with
// the same ValidationException shape still classifies as invalid_request.
func TestClassifyProviderErr_RouteMisconfig(t *testing.T) {
	routeErrs := []error{
		errors.New("operation error Bedrock Runtime: Converse, StatusCode: 400, ValidationException: Could not resolve the foundation model from the provided model identifier."),
		errors.New("ValidationException: The provided model identifier is invalid."),
		errors.New("the requested model is no longer supported"),
		&ports.ProviderError{Status: 404, Label: "openai_compatible", Body: "model not found"},
		&ports.ProviderError{Status: 400, Label: "bedrock", Body: "ValidationException: could not resolve the foundation model"},
	}
	for _, e := range routeErrs {
		if got := classifyProviderErr(e); got != reasonProviderConfig {
			t.Errorf("classifyProviderErr(%v) = %q, want %q", e, got, reasonProviderConfig)
		}
	}
	// A real parameter rejection (R_FUN001_haiku_temp_top_p shape) must stay invalid_request.
	paramErr := &ports.ProviderError{Status: 400, Label: "bedrock", Body: "ValidationException: temperature and top_p cannot both be specified"}
	if got := classifyProviderErr(paramErr); got != reasonInvalidRequest {
		t.Errorf("param rejection = %q, want %q", got, reasonInvalidRequest)
	}
	// provider_config is config/non-SLI.
	if cat, sli := reasonMeta(reasonProviderConfig); cat != catConfig || sli {
		t.Errorf("reasonMeta(provider_config) = (%q,%v), want (config,false)", cat, sli)
	}
}

// CORE-BND-008: capability rejections the provider phrases with "model … not supported"
// are CALLER faults (the request asked for tools/images the model lacks), not an
// unresolvable route. They must stay invalid_request — the old broad `(model|identifier)
// && not supported` rule mis-classified them as provider_config, and the bare
// Contains(e,"404") matched "404" anywhere in free text. Fails on the old code.
func TestClassifyProviderErr_CapabilityRejectionsAreCallerFaults(t *testing.T) {
	callerFaults := []error{
		&ports.ProviderError{Status: 400, Label: "bedrock", Body: "ValidationException: tool use is not supported by this model"},
		&ports.ProviderError{Status: 400, Label: "bedrock", Body: "ValidationException: image inputs are not supported by model anthropic.claude-x"},
		errors.New("ValidationException: this model does not support tool use (req id 404abc)"),
	}
	for _, e := range callerFaults {
		if got := classifyProviderErr(e); got != reasonInvalidRequest {
			t.Errorf("classifyProviderErr(%v) = %q, want %q (caller fault, not a route error)", e, got, reasonInvalidRequest)
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
