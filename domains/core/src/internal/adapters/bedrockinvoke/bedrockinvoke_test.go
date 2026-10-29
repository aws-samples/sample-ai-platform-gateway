// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package bedrockinvoke

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aiplat/core/internal/ports"
)

func f64(v float64) *float64 { return &v }

// --- buildBody (pure) --------------------------------------------------------

func TestBuildBody_SystemMessagesInference(t *testing.T) {
	in := ports.InvokeInput{
		Messages: []ports.Message{
			{Role: "system", Text: "be terse"},
			{Role: "user", Text: "hi"},
			{Role: "assistant", Text: "hello"},
			{Role: "user", Text: "bye"},
		},
		MaxOutputTokens: 64,
		Inference:       ports.InferenceParams{Temperature: f64(0)}, // 0 is meaningful, must be sent
	}
	b := buildBody(in, false)

	if len(b.System) != 1 || b.System[0].Text != "be terse" {
		t.Fatalf("system not mapped: %+v", b.System)
	}
	if len(b.Messages) != 3 {
		t.Fatalf("want 3 conversation turns, got %d", len(b.Messages))
	}
	if b.Messages[0].Role != "user" || b.Messages[0].Content[0].Text != "hi" {
		t.Fatalf("first turn wrong: %+v", b.Messages[0])
	}
	if b.Messages[1].Role != "assistant" || b.Messages[1].Content[0].Text != "hello" {
		t.Fatalf("assistant turn wrong: %+v", b.Messages[1])
	}
	if b.InferenceConfig == nil || b.InferenceConfig.MaxTokens == nil || *b.InferenceConfig.MaxTokens != 64 {
		t.Fatalf("maxTokens not set: %+v", b.InferenceConfig)
	}
	if b.InferenceConfig.Temperature == nil || *b.InferenceConfig.Temperature != 0 {
		t.Fatalf("temperature 0 must be sent, got %+v", b.InferenceConfig.Temperature)
	}
	// topP and stop were not given, so they must be absent from the JSON entirely.
	raw, _ := json.Marshal(b)
	if strings.Contains(string(raw), "topP") || strings.Contains(string(raw), "stopSequences") {
		t.Fatalf("absent params must not be emitted: %s", raw)
	}
}

func TestBuildBody_OmitsEmptyAndDefaults(t *testing.T) {
	in := ports.InvokeInput{Messages: []ports.Message{
		{Role: "assistant", Text: ""}, // empty assistant turn (e.g. only tool calls) is skipped
		{Role: "user", Text: "ping"},
	}}
	b := buildBody(in, false)
	if len(b.Messages) != 1 || b.Messages[0].Content[0].Text != "ping" {
		t.Fatalf("empty turn not skipped: %+v", b.Messages)
	}
	if b.InferenceConfig != nil {
		t.Fatalf("no inference params given, config must be nil, got %+v", b.InferenceConfig)
	}
}

func TestBuildBody_CachePointOnSystem(t *testing.T) {
	in := ports.InvokeInput{Messages: []ports.Message{
		{Role: "system", Text: "stable prefix"},
		{Role: "user", Text: "q"},
	}}
	b := buildBody(in, true)
	if len(b.System) != 2 || b.System[1].CachePoint == nil || b.System[1].CachePoint.Type != "default" {
		t.Fatalf("cache point not appended to system: %+v", b.System)
	}
	// With no system block, a cache point must NOT be invented.
	b2 := buildBody(ports.InvokeInput{Messages: []ports.Message{{Role: "user", Text: "q"}}}, true)
	for _, s := range b2.System {
		if s.CachePoint != nil {
			t.Fatalf("cache point must not appear without a system block")
		}
	}
}

// --- parseBody (pure) --------------------------------------------------------

func TestParseBody_ReportedCacheCounters(t *testing.T) {
	// Shape measured from the live gateway on 2026-10-26.
	body := []byte(`{"output":{"message":{"role":"assistant","content":[{"text":"good"}]}},` +
		`"stopReason":"end_turn","usage":{"inputTokens":6,"outputTokens":2,"totalTokens":8,` +
		`"cacheReadInputTokenCount":5,"cacheWriteInputTokenCount":0}}`)
	res, err := parseBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "good" || res.StopReason != "end_turn" {
		t.Fatalf("text/stop wrong: %+v", res)
	}
	if res.InputTokens != 6 || res.OutputTokens != 2 {
		t.Fatalf("tokens wrong: %+v", res)
	}
	if res.CacheReadInputTokens != 5 || res.CacheWriteInputTokens != 0 {
		t.Fatalf("cache tokens wrong: %+v", res)
	}
	if res.CacheCounters != ports.CacheCountersReported {
		t.Fatalf("usage present must be Reported, got %q", res.CacheCounters)
	}
}

func TestParseBody_MultipleTextBlocksAndNoUsage(t *testing.T) {
	body := []byte(`{"output":{"message":{"content":[{"text":"a"},{"text":"b"}]}},"stopReason":"end_turn"}`)
	res, err := parseBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "ab" {
		t.Fatalf("text blocks not concatenated: %q", res.Text)
	}
	if res.CacheCounters != ports.CacheCountersAbsent {
		t.Fatalf("no usage must be Absent, got %q", res.CacheCounters)
	}
}

// --- Invoke (signer seam + fake transport) -----------------------------------

type fakeSigner struct {
	service, region string
	payloadLen      int
	called          bool
}

func (s *fakeSigner) SignRequest(_ context.Context, req *http.Request, payload []byte, service, region string) error {
	s.called = true
	s.service, s.region, s.payloadLen = service, region, len(payload)
	req.Header.Set("authorization", "AWS4-HMAC-SHA256 test")
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newResp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func TestInvoke_SignsAndParses(t *testing.T) {
	GW := "https://gw-abc.gateway.bedrock-agentcore.us-east-1.amazonaws.com"
	sg := &fakeSigner{}
	var gotURL, gotAuth string
	var gotBody []byte
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		gotAuth = r.Header.Get("authorization")
		gotBody, _ = io.ReadAll(r.Body)
		return newResp(200, `{"output":{"message":{"content":[{"text":"ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":3,"outputTokens":1,"cacheReadInputTokenCount":0,"cacheWriteInputTokenCount":0}}`), nil
	})
	p, err := New(&http.Client{Transport: rt}, sg, Route{BaseURL: GW, ModelID: "us.amazon.nova-micro-v1:0"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Invoke(context.Background(), ports.InvokeInput{
		Messages: []ports.Message{{Role: "user", Text: "hi"}}, MaxOutputTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The ':' id must appear verbatim in the path.
	if !strings.Contains(gotURL, "/inference/model/us.amazon.nova-micro-v1:0/invoke") {
		t.Fatalf("invoke path wrong: %s", gotURL)
	}
	if !sg.called || sg.service != "bedrock-agentcore" || sg.region != "us-east-1" {
		t.Fatalf("signer not called correctly: %+v", sg)
	}
	if gotAuth == "" {
		t.Fatalf("request was not signed")
	}
	if sg.payloadLen != len(gotBody) {
		t.Fatalf("signed payload length %d != sent body length %d", sg.payloadLen, len(gotBody))
	}
	if res.Text != "ok" || res.InputTokens != 3 || res.CacheCounters != ports.CacheCountersReported {
		t.Fatalf("result wrong: %+v", res)
	}
}

func TestInvoke_HTTPErrorSurfacesStatus(t *testing.T) {
	GW := "https://gw-abc.gateway.bedrock-agentcore.us-east-1.amazonaws.com"
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return newResp(429, `{"message":"Too Many Requests"}`), nil
	})
	p, _ := New(&http.Client{Transport: rt}, &fakeSigner{}, Route{BaseURL: GW, ModelID: "us.amazon.nova-micro-v1:0"})
	_, err := p.Invoke(context.Background(), ports.InvokeInput{Messages: []ports.Message{{Role: "user", Text: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want error carrying status 429, got %v", err)
	}
}

func TestNew_Validations(t *testing.T) {
	GW := "https://gw-abc.gateway.bedrock-agentcore.us-east-1.amazonaws.com"
	if _, err := New(nil, &fakeSigner{}, Route{ModelID: "m"}); err == nil {
		t.Fatal("want error when base_url missing")
	}
	if _, err := New(nil, nil, Route{BaseURL: GW, ModelID: "m"}); err == nil {
		t.Fatal("want error when signer missing")
	}
	if _, err := New(nil, &fakeSigner{}, Route{BaseURL: GW}); err == nil {
		t.Fatal("want error when model id missing")
	}
	// Region derived from the host when not set.
	if _, err := New(nil, &fakeSigner{}, Route{BaseURL: GW, ModelID: "m"}); err != nil {
		t.Fatalf("region should derive from host: %v", err)
	}
}
