// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// CONTRACT test — the EMITTER side of the Usage_Record seam (XC-ARC-001 / OBS-BND-001).
// It drives a successful request and asserts the emitted record carries the schema stamp
// and every field the shared fixture marks as always-present. The SAME fixture is asserted
// on the writer side (observability/cmd/usage-writer) with no shared code.
package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiplat/core/internal/httpapi"
)

const usageContractPath = "../../../../../testdata/contracts/usage-record/record-v1.json"

func TestUsageRecordContract_Emitter(t *testing.T) {
	b, err := os.ReadFile(filepath.FromSlash(usageContractPath))
	if err != nil {
		t.Fatalf("could not read the contract fixture: %v", err)
	}
	var fx struct {
		Schema        string   `json:"schema"`
		EmitterAlways []string `json:"emitter_always"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}

	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tool_use":true,"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	recs := installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef, _ invocation) (result, error) {
		return result{text: "ok", tin: 100, tout: 50, reasoningChars: 529, cacheRead: 12, cacheWrite: 3, stopReason: "end_turn"}, nil
	})

	req := httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`,
		RequestID: "test-usage-contract",
	}
	resp, err := handle(context.Background(), req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("handle: err=%v status=%d", err, resp.StatusCode)
	}
	if len(*recs) != 1 {
		t.Fatalf("expected 1 Usage_Record, got %d", len(*recs))
	}
	rec := (*recs)[0]
	if rec["schema"] != fx.Schema {
		t.Errorf("emitted schema = %v, want %q", rec["schema"], fx.Schema)
	}
	for _, k := range fx.EmitterAlways {
		if _, ok := rec[k]; !ok {
			t.Errorf("emitter dropped always-present contract field %q", k)
		}
	}
	// The reasoning split must be present when the model reasoned (529 chars here).
	if _, ok := rec["reasoning_chars"]; !ok {
		t.Errorf("reasoning_chars must be emitted when the model reasoned")
	}
}
