// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package bedrock

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	btypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/aiplat/core/internal/ports"
)

func fp(v float64) *float64 { return &v }

// ── the defect ────────────────────────────────────────────────────────────────
//
// ConverseInput was built with four fields — ModelId, Messages, System, ToolConfig — and
// nothing else, on BOTH paths. InferenceConfig (where max_tokens, temperature, top_p and
// stop sequences live) was never set, so the client's parameters were parsed by the gateway
// and dropped here. AdditionalModelRequestFields, the only door Converse has for extended
// thinking, was never set either — which is why a thinking model could not be asked to think.

func TestApplyInference_MapsEveryParameter(t *testing.T) {
	cfg, extra := applyInference(ports.InvokeInput{
		MaxOutputTokens: 777,
		Inference: ports.InferenceParams{
			Temperature: fp(0.3),
			TopP:        fp(0.8),
			Stop:        []string{"STOP"},
		},
	})
	if cfg == nil {
		t.Fatal("InferenceConfig is nil: every parameter the client sent would be dropped")
	}
	if aws.ToInt32(cfg.MaxTokens) != 777 {
		t.Errorf("MaxTokens = %v, want 777", cfg.MaxTokens)
	}
	if aws.ToFloat32(cfg.Temperature) != 0.3 {
		t.Errorf("Temperature = %v, want 0.3", cfg.Temperature)
	}
	if aws.ToFloat32(cfg.TopP) != 0.8 {
		t.Errorf("TopP = %v, want 0.8", cfg.TopP)
	}
	if len(cfg.StopSequences) != 1 || cfg.StopSequences[0] != "STOP" {
		t.Errorf("StopSequences = %v, want [STOP]", cfg.StopSequences)
	}
	if extra != nil {
		t.Error("no thinking was requested; additionalModelRequestFields must stay absent")
	}
}

// A request that names nothing must produce NOTHING. Sending an invented zero is not a
// neutral act: temperature 0 makes the model deterministic and max_tokens 0 is rejected
// outright, so "the client said nothing" has to reach Bedrock as an absent field.
func TestApplyInference_AbsentMeansAbsent(t *testing.T) {
	cfg, extra := applyInference(ports.InvokeInput{})
	if cfg != nil {
		t.Errorf("InferenceConfig = %+v, want nil when the client sent no parameter", cfg)
	}
	if extra != nil {
		t.Errorf("additionalModelRequestFields = %v, want nil", extra)
	}
}

// temperature 0 is a real request for determinism and must survive as 0 — the exact case a
// non-pointer field would make indistinguishable from "unset".
func TestApplyInference_ZeroTemperatureIsSent(t *testing.T) {
	cfg, _ := applyInference(ports.InvokeInput{Inference: ports.InferenceParams{Temperature: fp(0)}})
	if cfg == nil || cfg.Temperature == nil {
		t.Fatal("temperature 0 was dropped; 0 is deterministic, not unset")
	}
	if aws.ToFloat32(cfg.Temperature) != 0 {
		t.Errorf("Temperature = %v, want 0", cfg.Temperature)
	}
}

// thinkingDoc marshals the free-form document back to JSON so the shape sent to Bedrock can
// be asserted the way the service will read it.
func thinkingDoc(t *testing.T, in ports.InvokeInput) map[string]interface{} {
	t.Helper()
	_, extra := applyInference(in)
	if extra == nil {
		return nil
	}
	b, err := extra.MarshalSmithyDocument()
	if err != nil {
		t.Fatalf("marshalling the document: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("document is not JSON: %v\n%s", err, string(b))
	}
	return out
}

func TestApplyInference_ThinkingTravelsInAdditionalModelRequestFields(t *testing.T) {
	doc := thinkingDoc(t, ports.InvokeInput{
		MaxOutputTokens: 8192,
		Inference: ports.InferenceParams{
			Reasoning: &ports.ReasoningRequest{Effort: "medium", BudgetTokens: 4096},
		},
	})
	th, ok := doc["thinking"].(map[string]interface{})
	if !ok {
		t.Fatalf("no thinking block: %v", doc)
	}
	if th["type"] != "enabled" {
		t.Errorf("thinking.type = %v, want enabled", th["type"])
	}
	if th["budget_tokens"] != float64(4096) {
		t.Errorf("thinking.budget_tokens = %v, want 4096", th["budget_tokens"])
	}
}

// A budget of zero, or an explicit disable, must send NO thinking field. Bedrock has no "off"
// switch for a model that reasons intrinsically, so the honest behaviour is to send only what
// the provider accepts and never pretend to have disabled something we cannot.
func TestApplyInference_DisabledOrZeroBudgetSendsNoThinking(t *testing.T) {
	cases := map[string]*ports.ReasoningRequest{
		"explicitly disabled": {Effort: "none", Disabled: true},
		"zero budget":         {Effort: "low", BudgetTokens: 0},
	}
	for name, r := range cases {
		if doc := thinkingDoc(t, ports.InvokeInput{Inference: ports.InferenceParams{Reasoning: r}}); doc != nil {
			t.Errorf("%s: additionalModelRequestFields must be absent, got %v", name, doc)
		}
	}
}

// ── both request paths, in lockstep ───────────────────────────────────────────

// captureConverse records the ConverseInput the BUFFERED path builds.
type captureConverse struct {
	in *bedrockruntime.ConverseInput
}

func (c *captureConverse) Converse(_ context.Context, params *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	c.in = params
	return &bedrockruntime.ConverseOutput{
		Output: &btypes.ConverseOutputMemberMessage{Value: btypes.Message{
			Content: []btypes.ContentBlock{&btypes.ContentBlockMemberText{Value: "ok"}},
		}},
		StopReason: btypes.StopReasonEndTurn,
		Usage:      &btypes.TokenUsage{InputTokens: aws.Int32(1), OutputTokens: aws.Int32(1)},
	}, nil
}

// The CALL SITE, not just the helper. Asserting applyInference in isolation left this
// untested: deleting the two lines that attach its output to ConverseInput kept the suite
// green, so every parameter could be dropped again without a single test noticing. Measured —
// that experiment was run.
func TestCallBedrock_AttachesInferenceToTheRequest(t *testing.T) {
	cc := &captureConverse{}
	in := ports.InvokeInput{
		Messages:        []ports.Message{{Role: "user", Text: "hi"}},
		MaxOutputTokens: 444,
		Inference: ports.InferenceParams{
			Temperature: fp(0.7),
			Reasoning:   &ports.ReasoningRequest{Effort: "low", BudgetTokens: 1024},
		},
	}
	if _, err := callBedrock(context.Background(), cc, "model-x", in, false); err != nil {
		t.Fatalf("callBedrock: %v", err)
	}
	if cc.in == nil {
		t.Fatal("Converse was never called")
	}
	if cc.in.InferenceConfig == nil {
		t.Fatal("the buffered path attached no InferenceConfig: the client's parameters are dropped")
	}
	if aws.ToInt32(cc.in.InferenceConfig.MaxTokens) != 444 {
		t.Errorf("MaxTokens = %v, want 444", cc.in.InferenceConfig.MaxTokens)
	}
	if aws.ToFloat32(cc.in.InferenceConfig.Temperature) != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", cc.in.InferenceConfig.Temperature)
	}
	if cc.in.AdditionalModelRequestFields == nil {
		t.Error("the buffered path sent no thinking block")
	}
}

// captureStream records the ConverseStreamInput the adapter builds.
type captureStream struct {
	in *bedrockruntime.ConverseStreamInput
}

func (c *captureStream) ConverseStream(_ context.Context, params *bedrockruntime.ConverseStreamInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	c.in = params
	// Returning no event stream makes openStream fail AFTER building the input, which is all
	// this test needs — and it needs no live client to do it.
	return &bedrockruntime.ConverseStreamOutput{}, nil
}

// ConverseInput and ConverseStreamInput are DISTINCT SDK types with identical members, so two
// separate literals would drift — and the half that drifts is whichever one has less
// coverage. This asserts the streaming path carries the same inference config as the buffered
// one, which is the invariant the shared applyInference helper exists to hold.
func TestOpenStream_CarriesTheSameInferenceConfig(t *testing.T) {
	cs := &captureStream{}
	in := ports.InvokeInput{
		Messages:        []ports.Message{{Role: "user", Text: "hi"}},
		MaxOutputTokens: 555,
		Inference: ports.InferenceParams{
			Temperature: fp(0.1),
			Reasoning:   &ports.ReasoningRequest{Effort: "low", BudgetTokens: 1024},
		},
	}
	// The error is expected (no event stream); the input is what is under test.
	_, _ = openStream(context.Background(), cs, "model-x", in, false)
	if cs.in == nil {
		t.Fatal("ConverseStream was never called")
	}
	if cs.in.InferenceConfig == nil {
		t.Fatal("the streaming path built no InferenceConfig — it drifted from the buffered path")
	}
	if aws.ToInt32(cs.in.InferenceConfig.MaxTokens) != 555 {
		t.Errorf("MaxTokens = %v, want 555", cs.in.InferenceConfig.MaxTokens)
	}
	if aws.ToFloat32(cs.in.InferenceConfig.Temperature) != 0.1 {
		t.Errorf("Temperature = %v, want 0.1", cs.in.InferenceConfig.Temperature)
	}
	if cs.in.AdditionalModelRequestFields == nil {
		t.Error("the streaming path sent no thinking block; a streaming reasoning request would not think")
	}
}
