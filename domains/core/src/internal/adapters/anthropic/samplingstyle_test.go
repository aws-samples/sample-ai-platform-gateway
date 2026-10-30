// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/aiplat/core/internal/ports"
)

func fptr(v float64) *float64 { return &v }

func sentBody(t *testing.T, style string, in ports.InvokeInput) map[string]interface{} {
	t.Helper()
	a := &Adapter{ModelID: "m", Transport: Transport{SamplingStyle: style}}
	req, err := a.buildRequest(context.Background(), in, false)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	b, _ := io.ReadAll(req.Body)
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body not JSON: %v\n%s", err, b)
	}
	return m
}

// An adaptive route omits temperature (the newer models reject it) but still sends top_p.
func TestSamplingStyle_AdaptiveOmitsTemperatureKeepsTopP(t *testing.T) {
	in := ports.InvokeInput{
		MaxOutputTokens: 16,
		Messages:        []ports.Message{{Role: "user", Text: "hi"}},
		Inference:       ports.InferenceParams{Temperature: fptr(0), TopP: fptr(0.9)},
	}
	m := sentBody(t, SamplingStyleAdaptive, in)
	if _, ok := m["temperature"]; ok {
		t.Errorf("adaptive route must omit temperature, body=%v", m)
	}
	if m["top_p"] != 0.9 {
		t.Errorf("top_p must still be sent, got %v", m["top_p"])
	}
}

// The default (empty) style keeps temperature — unchanged behaviour for existing models.
func TestSamplingStyle_DefaultSendsTemperature(t *testing.T) {
	in := ports.InvokeInput{
		MaxOutputTokens: 16,
		Messages:        []ports.Message{{Role: "user", Text: "hi"}},
		Inference:       ports.InferenceParams{Temperature: fptr(0)},
	}
	m := sentBody(t, "", in)
	if _, ok := m["temperature"]; !ok {
		t.Errorf("default route must send temperature (0 is a real request), body=%v", m)
	}
}
