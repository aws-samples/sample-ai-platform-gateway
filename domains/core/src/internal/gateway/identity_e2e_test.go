// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Orchestration tests for model identity in the router shell (task 8).
//
// These exercise handle() end to end with a provider double, because the two
// behaviors they cover only exist once the shell wiring is in place: the config
// fields have to reach Candidate/Policy, and the outcome has to reach the
// Usage_Record and the client response. The pure domain tests
// (internal/routing/identity_decide_test.go) already prove the decision itself.
package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aiplat/core/internal/httpapi"
	"github.com/aiplat/core/internal/ports"
)

// fakeConfig injects an effective config that envConfig cannot express — only
// MODEL_ROUTING and PRICING_TABLE come from the environment, while identity and
// swap policy live in fields the Governance table supplies in production.
type fakeConfig struct{ extra map[string]interface{} }

func (f *fakeConfig) Effective(_ context.Context, team, app string, base map[string]interface{}) (map[string]interface{}, string, string) {
	for k, v := range f.extra {
		base[k] = v
	}
	return base, "ORG", "ORG"
}

var _ ports.ConfigStore = (*fakeConfig)(nil)

// aiplatBlock extracts the gateway metadata block from a response body.
func aiplatBlock(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	m, ok := out["aiplat"].(map[string]interface{})
	if !ok {
		t.Fatalf("response has no aiplat block: %s", body)
	}
	return m
}

// TestE2E_ProviderArbitrage: two routes declare the same model_id at different
// prices. With cost optimization on, the cheaper one serves and the saving is
// VERIFIED — the only place in the ledger where a substitution earns that label,
// because the model served is the model requested.
func TestE2E_ProviderArbitrage(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{
		"gpt-openai":{"provider":"openai_compatible","provider_model_id":"gpt-5.2","model_id":"openai/gpt-5.2","capabilities":{"tier":"frontier"}},
		"gpt-azure":{"provider":"openai_compatible","provider_model_id":"gpt-5.2","model_id":"openai/gpt-5.2","capabilities":{"tier":"frontier"}}
	}`)
	t.Setenv("PRICING_TABLE", `{
		"gpt-openai":{"input":0.010,"output":0.030},
		"gpt-azure":{"input":0.005,"output":0.015}
	}`)
	configStore = &fakeConfig{extra: map[string]interface{}{"auto_cheapest": true}}

	recs := installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef) (result, error) {
		return result{text: "ok", tin: 1000, tout: 200}, nil
	})

	resp, err := handle(context.Background(), httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"gpt-openai","messages":[{"role":"user","content":"oi"}]}`,
		RequestID: "test-arbitrage",
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, resp.Body)
	}

	meta := aiplatBlock(t, resp.Body)
	if meta["model"] != "gpt-azure" {
		t.Errorf("served model = %v, want gpt-azure (the cheaper route of the same group)", meta["model"])
	}
	if meta["swap_class"] != "same_model" {
		t.Errorf("swap_class = %v, want same_model", meta["swap_class"])
	}
	if meta["served_model_id"] != "openai/gpt-5.2" {
		t.Errorf("served_model_id = %v, want openai/gpt-5.2", meta["served_model_id"])
	}
	if meta["savings_reason"] != "provider_arbitrage" {
		t.Errorf("savings_reason = %v, want provider_arbitrage", meta["savings_reason"])
	}
	if meta["savings_class"] != "verified" {
		t.Errorf("savings_class = %v, want verified", meta["savings_class"])
	}

	if len(*recs) != 1 {
		t.Fatalf("want 1 Usage_Record, got %d", len(*recs))
	}
	rec := (*recs)[0]
	if rec["swap_class"] != "same_model" || rec["served_model_id"] != "openai/gpt-5.2" {
		t.Errorf("Usage_Record swap fields = %v / %v", rec["swap_class"], rec["served_model_id"])
	}
	// The whole point of the verified column: this saving has no assumed baseline,
	// so it must not be filed as counterfactual.
	if v, _ := rec["saved_verified_usd"].(float64); v <= 0 {
		t.Errorf("saved_verified_usd = %v, want > 0 (arbitrage is verified saving)", rec["saved_verified_usd"])
	}
	if v, _ := rec["saved_counterfactual_usd"].(float64); v != 0 {
		t.Errorf("saved_counterfactual_usd = %v, want 0", v)
	}
}

// TestE2E_SwapNotAllowed: the feature declares same_model_only, the requested
// route cannot serve this request, and the only route that can is a DIFFERENT
// model. The gateway refuses instead of silently trading response depth — and the
// refusal is classified as policy, not as a reliability failure of ours.
//
// The request carries tools and the requested route declares none, which is how
// the requested route becomes unservable WITHOUT tripping the allowed_models check
// that runs before the routing decision.
func TestE2E_SwapNotAllowed(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{
		"gpt-openai":{"provider":"openai_compatible","provider_model_id":"gpt-5.2","model_id":"openai/gpt-5.2","capabilities":{"tier":"frontier"}},
		"cheap-haiku":{"provider":"anthropic","provider_model_id":"haiku","model_id":"anthropic/haiku","capabilities":{"tool_use":true,"tier":"fast"}}
	}`)
	t.Setenv("PRICING_TABLE", `{
		"gpt-openai":{"input":0.010,"output":0.030},
		"cheap-haiku":{"input":0.0001,"output":0.0004}
	}`)
	configStore = &fakeConfig{extra: map[string]interface{}{
		"feature_policy": map[string]interface{}{
			"reasoning": map[string]interface{}{"swap": "same_model_only"},
		},
	}}

	providerCalled := false
	recs := installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef) (result, error) {
		providerCalled = true
		return result{text: "ok", tin: 10, tout: 5}, nil
	})

	resp, err := handle(context.Background(), httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers: map[string]string{"authorization": "Bearer test"},
		Body: `{"model":"gpt-openai","feature":"reasoning","messages":[{"role":"user","content":"oi"}],
			"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`,
		RequestID: "test-swap-blocked",
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, resp.Body)
	}
	if providerCalled {
		t.Error("a declared failure must not reach any provider")
	}
	var errBody struct {
		Error struct{ Code string } `json:"error"`
	}
	json.Unmarshal([]byte(resp.Body), &errBody)
	// A distinct code is the whole point: no_eligible_model would tell the customer
	// to fix the catalog, when what they must do is loosen the policy.
	if errBody.Error.Code != "swap_not_allowed" {
		t.Errorf("error code = %q, want swap_not_allowed", errBody.Error.Code)
	}

	if len(*recs) != 1 {
		t.Fatalf("want 1 Usage_Record for the block, got %d", len(*recs))
	}
	rec := (*recs)[0]
	if rec["status"] != "blocked" || rec["reason"] != "swap_not_allowed" {
		t.Errorf("record status/reason = %v / %v, want blocked / swap_not_allowed", rec["status"], rec["reason"])
	}
	if rec["category"] != "policy" {
		t.Errorf("category = %v, want policy", rec["category"])
	}
	if rec["sli_eligible"] != false {
		t.Errorf("sli_eligible = %v, want false — obeying the policy is not our outage", rec["sli_eligible"])
	}
}
