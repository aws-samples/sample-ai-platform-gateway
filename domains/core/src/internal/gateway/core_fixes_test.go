// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"encoding/json"
	"testing"
)

// CORE-BND-003: unsupported OpenAI parameters are rejected (named), not silently dropped.
func TestUnsupportedParam(t *testing.T) {
	n3 := 3
	n1 := 1
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	cases := []struct {
		name       string
		n          *int
		rf, tc, lb json.RawMessage
		wantField  string
	}{
		{"n>1", &n3, nil, nil, nil, "n"},
		{"n=1 ok", &n1, nil, nil, nil, ""},
		{"response_format", nil, raw(`{"type":"json_object"}`), nil, nil, "response_format"},
		{"logit_bias", nil, nil, nil, raw(`{"50256":-100}`), "logit_bias"},
		{"tool_choice required", nil, nil, raw(`"required"`), nil, "tool_choice"},
		{"tool_choice object", nil, nil, raw(`{"type":"function"}`), nil, "tool_choice"},
		{"tool_choice auto ok", nil, nil, raw(`"auto"`), nil, ""},
		{"all absent", nil, nil, nil, nil, ""},
	}
	for _, c := range cases {
		got, _ := unsupportedParam(c.n, c.rf, c.tc, c.lb)
		if got != c.wantField {
			t.Errorf("%s: got field %q, want %q", c.name, got, c.wantField)
		}
	}
}

// CORE-NFR-004: a provider error that means "this model cannot stream" is distinguished
// from a real failure, so only the former falls back buffered on the same route.
func TestStreamingUnsupported(t *testing.T) {
	yes := []string{
		"ValidationException: streaming is not supported for this model",
		"this model does not support streaming",
		"provider has no native stream",
	}
	for _, s := range yes {
		if !streamingUnsupported(errString(s)) {
			t.Errorf("expected %q to be recognised as streaming-unsupported", s)
		}
	}
	no := []string{"ThrottlingException: rate exceeded", "AccessDenied", "dial tcp: timeout"}
	for _, s := range no {
		if streamingUnsupported(errString(s)) {
			t.Errorf("expected %q NOT to be treated as streaming-unsupported", s)
		}
	}
}

// CORE-SEC-006: the caller-facing 429 scope label never exposes the internal storage key.
func TestScopeLabel(t *testing.T) {
	if got := scopeLabel("alpha", "web"); got != "team=alpha app=web" {
		t.Errorf("got %q", got)
	}
	if got := scopeLabel("", ""); got != "team=default app=default" {
		t.Errorf("empty parts: got %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
