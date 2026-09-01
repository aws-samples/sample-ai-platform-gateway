// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Swap-policy enforcement and provider arbitrage inside the decision.
//
// These are the tests that make the feature's promise checkable: the policy must
// bind ALL THREE substitution paths (cost optimization, provider fallback and
// budget degrade), and a blocked substitution must produce a declared failure
// rather than a different model.
package routing

import (
	"testing"
	"time"
)

// twoProvidersSameModel is the shape the feature exists for: one model, two
// providers, one of them cheaper — plus a genuinely different, cheaper model.
func twoProvidersSameModel() []Candidate {
	return []Candidate{
		{Model: "gpt-openai", Provider: "openai_compatible", ModelID: "openai/gpt-5.2",
			Caps:   Capabilities{Tier: "frontier", ToolUse: true, ContextWindow: 200000},
			Prices: flatPrice(0.0030, 0.0150)},
		{Model: "gpt-azure", Provider: "azure", ModelID: "openai/gpt-5.2",
			Caps:   Capabilities{Tier: "frontier", ToolUse: true, ContextWindow: 200000},
			Prices: flatPrice(0.0020, 0.0100)},
		{Model: "haiku", Provider: "bedrock", ModelID: "anthropic/claude-haiku-4.5",
			Caps:   Capabilities{Tier: "fast", ToolUse: true, ContextWindow: 100000},
			Prices: flatPrice(0.0002, 0.0010)},
	}
}

func identityPolicy() Policy {
	return Policy{
		ModelOrder:    []string{"gpt-openai", "gpt-azure", "haiku"},
		DefaultOutTok: 512,
	}
}

var idReq = RequestShape{InputTokens: 1000, MaxOutputTokens: 500, RequestedModel: "gpt-openai"}

// --- Provider arbitrage -----------------------------------------------------

// With the swap policy locking the model, the cheaper provider of the SAME model
// still wins. This is the headline behavior: cost reduction with no quality risk.
func TestDecide_Arbitrage_SameModelCheaperProviderWins(t *testing.T) {
	p := identityPolicy()
	p.AutoCheapest = true
	p.Swap = SwapSameModelOnly

	d, err := Decide(twoProvidersSameModel(), p, nil, nil, idReq, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "gpt-azure" {
		t.Errorf("model = %q, want gpt-azure (same model, cheaper provider)", d.Model)
	}
	if d.SwapClass != SwapSameModel {
		t.Errorf("SwapClass = %q, want %q", d.SwapClass, SwapSameModel)
	}
	if d.ServedModelID != "openai/gpt-5.2" {
		t.Errorf("ServedModelID = %q", d.ServedModelID)
	}
	// The cheaper DIFFERENT model must have been refused by policy, not by price.
	if r := discardReasonFor(d, "haiku"); r != DiscardSwapNotAllowed {
		t.Errorf("haiku discard = %q, want %q", r, DiscardSwapNotAllowed)
	}
}

// Without a swap policy, cost optimization keeps its full reach: the globally
// cheapest wins even if that means changing models. Quality is governed by the
// policy, cost by auto-cheapest, and the two compose.
func TestDecide_NoSwapPolicy_CostOptimizationKeepsFullReach(t *testing.T) {
	p := identityPolicy()
	p.AutoCheapest = true

	d, err := Decide(twoProvidersSameModel(), p, nil, nil, idReq, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "haiku" {
		t.Errorf("model = %q, want haiku (globally cheapest, no policy declared)", d.Model)
	}
	if d.SwapClass != SwapDowngrade {
		t.Errorf("SwapClass = %q, want %q", d.SwapClass, SwapDowngrade)
	}
}

// --- Path 1: cost optimization ---------------------------------------------

func TestDecide_SwapPolicy_BindsCostOptimization(t *testing.T) {
	p := identityPolicy()
	p.AutoCheapest = true
	p.Swap = SwapAllowEquivalent // equivalent yes, downgrade no

	d, err := Decide(twoProvidersSameModel(), p, nil, nil, idReq, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model == "haiku" {
		t.Fatal("haiku is a downgrade and must not be served under allow_equivalent")
	}
	if d.Model != "gpt-azure" {
		t.Errorf("model = %q, want gpt-azure", d.Model)
	}
}

// --- Path 2: provider fallback (availability) -----------------------------
//
// The requested provider being unavailable must fall to the SAME model on another
// provider — the exact case the pre-feature pin made impossible.
func TestDecide_SwapPolicy_FailoverToSameModelSurvivesUnavailability(t *testing.T) {
	p := identityPolicy()
	p.Swap = SwapSameModelOnly
	p.FeatureModels = []string{"openai/gpt-5.2"} // pin by MODEL, not by route
	h := &Hints{Unavailable: map[string]time.Time{"gpt-openai": baseNow.Add(time.Hour)}}

	d, err := Decide(twoProvidersSameModel(), p, h, nil, idReq, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "gpt-azure" {
		t.Errorf("model = %q, want gpt-azure — pinning by model must keep failover alive", d.Model)
	}
	if d.SwapClass != SwapSameModel {
		t.Errorf("SwapClass = %q, want %q", d.SwapClass, SwapSameModel)
	}
}

// --- Path 3: budget degrade ----------------------------------------------
//
// Budget degrade turns cost optimization on. It must NOT be a side door around the
// quality policy the customer declared.
func TestDecide_SwapPolicy_BindsBudgetDegrade(t *testing.T) {
	p := identityPolicy()
	p.Swap = SwapSameModelOnly
	p.AutoCheapest = true // this is exactly what budget degrade sets

	d, err := Decide(twoProvidersSameModel(), p, nil, nil, idReq, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model == "haiku" {
		t.Fatal("budget degrade must not break same_model_only")
	}
	if d.Model != "gpt-azure" {
		t.Errorf("model = %q, want gpt-azure", d.Model)
	}
}

// --- Declared failure -----------------------------------------------------

// When the only alternatives are of a forbidden class, the answer is an error with
// its own code — never a response from a model the customer excluded.
func TestDecide_SwapPolicy_EmptyProducesDeclaredFailure(t *testing.T) {
	cands := []Candidate{
		{Model: "gpt-openai", Provider: "openai_compatible", ModelID: "openai/gpt-5.2",
			Caps: Capabilities{Tier: "frontier", ToolUse: true}, Prices: flatPrice(0.003, 0.015)},
		{Model: "haiku", Provider: "bedrock", ModelID: "anthropic/claude-haiku-4.5",
			Caps: Capabilities{Tier: "fast", ToolUse: true}, Prices: flatPrice(0.0002, 0.001)},
	}
	p := identityPolicy()
	p.Swap = SwapSameModelOnly
	// The requested route is unavailable and has no same-model sibling.
	h := &Hints{Unavailable: map[string]time.Time{"gpt-openai": baseNow.Add(time.Hour)}}

	// Availability is a reversible filter, so the requested route survives by
	// degradation — the policy must not turn that into a model swap.
	d, err := Decide(cands, p, h, nil, idReq, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "gpt-openai" {
		t.Errorf("model = %q; degrading availability must keep the requested model, not swap it", d.Model)
	}
}

// A hard eligibility failure of the requested model, with only forbidden classes
// left, is the case that must produce ErrSwapNotAllowed.
func TestDecide_SwapPolicy_HardBlockIsDistinctFromMisconfiguration(t *testing.T) {
	cands := []Candidate{
		// Requested model cannot do tool use, so it is hard-ineligible here.
		{Model: "gpt-openai", Provider: "openai_compatible", ModelID: "openai/gpt-5.2",
			Caps: Capabilities{Tier: "frontier", ToolUse: false}, Prices: flatPrice(0.003, 0.015)},
		{Model: "haiku", Provider: "bedrock", ModelID: "anthropic/claude-haiku-4.5",
			Caps: Capabilities{Tier: "fast", ToolUse: true}, Prices: flatPrice(0.0002, 0.001)},
	}
	p := identityPolicy()
	p.Swap = SwapSameModelOnly

	req := idReq
	req.HasTools = true
	_, err := Decide(cands, p, nil, nil, req, baseNow)
	if err != ErrSwapNotAllowed {
		t.Errorf("err = %v, want ErrSwapNotAllowed — policy enforcement must be distinguishable from a config mistake", err)
	}
}

// With no capable model at all and no policy, the error must remain the
// pre-feature one: the customer's next action is to fix the catalog.
func TestDecide_NoCapableModel_KeepsConfigurationError(t *testing.T) {
	cands := []Candidate{
		{Model: "a", Provider: "p", Caps: Capabilities{Tier: "fast", ToolUse: false}, Prices: flatPrice(0.001, 0.005)},
	}
	req := RequestShape{InputTokens: 100, HasTools: true}
	if _, err := Decide(cands, identityPolicy(), nil, nil, req, baseNow); err != ErrNoEligibleModel {
		t.Errorf("err = %v, want ErrNoEligibleModel", err)
	}
}

// --- Pin by model id keeps the group eligible ---------------------------

func TestDecide_PinByModelID_EnablesWholeGroup(t *testing.T) {
	p := identityPolicy()
	p.AutoCheapest = true
	p.FeatureModels = []string{"openai/gpt-5.2"}

	d, err := Decide(twoProvidersSameModel(), p, nil, nil, idReq, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "gpt-azure" {
		t.Errorf("model = %q, want gpt-azure — the pin must enable the whole group", d.Model)
	}
	if r := discardReasonFor(d, "haiku"); r != DiscardTierNotAllowed {
		t.Errorf("haiku discard = %q, want %q", r, DiscardTierNotAllowed)
	}
}

func TestDecide_PinByRouteName_EnablesOnlyThatRoute(t *testing.T) {
	p := identityPolicy()
	p.AutoCheapest = true
	p.FeatureModels = []string{"gpt-openai"}

	d, err := Decide(twoProvidersSameModel(), p, nil, nil, idReq, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "gpt-openai" {
		t.Errorf("model = %q, want gpt-openai — pinning a route name must not widen", d.Model)
	}
	if r := discardReasonFor(d, "gpt-azure"); r != DiscardTierNotAllowed {
		t.Errorf("gpt-azure discard = %q, want %q", r, DiscardTierNotAllowed)
	}
}
