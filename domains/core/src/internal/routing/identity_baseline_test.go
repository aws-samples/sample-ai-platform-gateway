// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// SAFETY NET for the model-identity-bundles feature (task 1).
//
// This suite freezes, as a golden, the routing decision for a catalog that does
// NOT declare any of the feature's new fields (model_id, aggregator, swap,
// bundle, canary).
//
// Why it comes before any other line of the feature: the promise that makes the
// feature adoptable is "an existing config decides exactly as before". That
// promise covers the hottest path in the product — the model choice of every
// request — and without a golden frozen BEFORE the change it is an opinion, not
// a fact.
//
// Property 1 of the design: absence of identity preserves current behavior.
package routing

import (
	"testing"
	"time"
)

var baseNow = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

func flatPrice(in, out Money) PriceHistory {
	return PriceHistory{{Standard: Layer{Input: in, Output: out}}}
}

// legacyCatalog is a catalog WITHOUT model_id/aggregator — the state of every org
// today. Prices differ so cost optimization has something to choose from.
func legacyCatalog() []Candidate {
	return []Candidate{
		{Model: "frontier-pricey", Provider: "bedrock",
			Caps:   Capabilities{Tier: "frontier", ToolUse: true, Multimodal: true, ContextWindow: 200000},
			Prices: flatPrice(0.0030, 0.0150)},
		{Model: "balanced-mid", Provider: "openai_compatible",
			Caps:   Capabilities{Tier: "balanced", ToolUse: true, Multimodal: false, ContextWindow: 100000},
			Prices: flatPrice(0.0010, 0.0050)},
		{Model: "fast-cheap", Provider: "openai_compatible",
			Caps:   Capabilities{Tier: "fast", ToolUse: false, Multimodal: false, ContextWindow: 8000},
			Prices: flatPrice(0.0001, 0.0005)},
	}
}

func legacyPolicy() Policy {
	return Policy{
		ModelOrder:    []string{"frontier-pricey", "balanced-mid", "fast-cheap"},
		DefaultOutTok: 512,
	}
}

// discardReasonFor returns the discard reason for a model, or "" if it survived.
func discardReasonFor(d Decision, model string) string {
	for _, x := range d.Discards {
		if x.Model == model {
			return x.Reason
		}
	}
	return ""
}

// --- Golden 1: without auto-cheapest, the REQUESTED model is served -----------
//
// This is the strongest guarantee for a customer who tuned a prompt: with no
// opt-in to optimization, the gateway does not swap models for price.
func TestBaseline_NoAutoCheapest_ServesRequested(t *testing.T) {
	d, err := Decide(legacyCatalog(), legacyPolicy(), nil, nil,
		RequestShape{InputTokens: 1000, MaxOutputTokens: 500, RequestedModel: "frontier-pricey"}, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "frontier-pricey" {
		t.Errorf("model = %q, requested model must be served without auto-cheapest", d.Model)
	}
}

// --- Golden 2: with auto-cheapest, the cheapest ELIGIBLE model wins ----------
func TestBaseline_AutoCheapest_PicksCheapest(t *testing.T) {
	p := legacyPolicy()
	p.AutoCheapest = true
	d, err := Decide(legacyCatalog(), p, nil, nil,
		RequestShape{InputTokens: 1000, MaxOutputTokens: 500, RequestedModel: "frontier-pricey"}, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "fast-cheap" {
		t.Errorf("model = %q, want fast-cheap (cheapest and eligible)", d.Model)
	}
	// The ledger baseline is the REQUESTED model, not whatever survived.
	if d.RequestedCostUSD <= d.ExpectedCostUSD {
		t.Errorf("baseline (%v) must exceed served cost (%v)", d.RequestedCostUSD, d.ExpectedCostUSD)
	}
}

// --- Golden 3: eligibility beats optimization (the historical bug) -----------
//
// A request carrying tools must not land on the cheapest model that cannot do
// tool use.
func TestBaseline_ToolUse_DiscardsIncapableEvenIfCheapest(t *testing.T) {
	p := legacyPolicy()
	p.AutoCheapest = true
	d, err := Decide(legacyCatalog(), p, nil, nil,
		RequestShape{InputTokens: 1000, MaxOutputTokens: 500, HasTools: true}, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model == "fast-cheap" {
		t.Fatal("a model without tool use must never serve a request carrying tools")
	}
	if r := discardReasonFor(d, "fast-cheap"); r != DiscardNoToolUse {
		t.Errorf("fast-cheap discard = %q, want %q", r, DiscardNoToolUse)
	}
	if d.Model != "balanced-mid" {
		t.Errorf("model = %q, want balanced-mid (cheapest WITH tool use)", d.Model)
	}
}

// --- Golden 4: insufficient context window discards --------------------------
func TestBaseline_ContextTooSmall_Discards(t *testing.T) {
	p := legacyPolicy()
	p.AutoCheapest = true
	d, err := Decide(legacyCatalog(), p, nil, nil,
		RequestShape{InputTokens: 50000, MaxOutputTokens: 1000}, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if r := discardReasonFor(d, "fast-cheap"); r != DiscardContextTooSmall {
		t.Errorf("fast-cheap discard = %q, want %q", r, DiscardContextTooSmall)
	}
	if d.Model != "balanced-mid" {
		t.Errorf("model = %q, want balanced-mid", d.Model)
	}
}

// --- Golden 5: multimodal discards models that do not declare it ------------
func TestBaseline_Multimodal_DiscardsUndeclared(t *testing.T) {
	p := legacyPolicy()
	p.AutoCheapest = true
	d, err := Decide(legacyCatalog(), p, nil, nil,
		RequestShape{InputTokens: 1000, MaxOutputTokens: 500, HasImage: true}, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "frontier-pricey" {
		t.Errorf("model = %q, only frontier-pricey is multimodal", d.Model)
	}
	if r := discardReasonFor(d, "balanced-mid"); r != DiscardNotMultimodal {
		t.Errorf("discard = %q, want %q", r, DiscardNotMultimodal)
	}
}

// --- Golden 6: allowed_models narrows the pool -----------------------------
func TestBaseline_AllowedModels_Narrows(t *testing.T) {
	p := legacyPolicy()
	p.AutoCheapest = true
	p.AllowedModels = []string{"frontier-pricey", "balanced-mid"}
	d, err := Decide(legacyCatalog(), p, nil, nil,
		RequestShape{InputTokens: 1000, MaxOutputTokens: 500}, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "balanced-mid" {
		t.Errorf("model = %q, want balanced-mid", d.Model)
	}
	if r := discardReasonFor(d, "fast-cheap"); r != DiscardNotAllowed {
		t.Errorf("discard = %q, want %q", r, DiscardNotAllowed)
	}
}

// --- Golden 7: pin by ROUTE NAME (pre-feature behavior) --------------------
//
// This is the golden that task 4 must preserve when the pin starts accepting a
// Model_ID: a policy declaring a route name keeps enabling ONLY that route.
func TestBaseline_FeatureModels_ByRouteName(t *testing.T) {
	p := legacyPolicy()
	p.AutoCheapest = true
	p.FeatureModels = []string{"frontier-pricey"}
	d, err := Decide(legacyCatalog(), p, nil, nil,
		RequestShape{InputTokens: 1000, MaxOutputTokens: 500, Feature: "reasoning"}, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "frontier-pricey" {
		t.Errorf("model = %q, the pin must lock on frontier-pricey", d.Model)
	}
	if r := discardReasonFor(d, "fast-cheap"); r != DiscardTierNotAllowed {
		t.Errorf("discard = %q, want %q", r, DiscardTierNotAllowed)
	}
}

// --- Golden 8: feature_tiers as a floor; economy mode opens DOWNWARD only ---
func TestBaseline_FeatureTiers_EconomyOpensDownwardOnly(t *testing.T) {
	p := legacyPolicy()
	p.AutoCheapest = true
	p.FeatureTiers = []string{"balanced"}

	// Without economy mode: only balanced is eligible.
	d, err := Decide(legacyCatalog(), p, nil, nil,
		RequestShape{InputTokens: 1000, MaxOutputTokens: 500}, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "balanced-mid" {
		t.Errorf("without economy: model = %q, want balanced-mid", d.Model)
	}

	// With economy mode: a LOWER tier opens up, never a higher one.
	p.EconomyMode = true
	d2, err := Decide(legacyCatalog(), p, nil, nil,
		RequestShape{InputTokens: 1000, MaxOutputTokens: 500}, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d2.Model != "fast-cheap" {
		t.Errorf("with economy: model = %q, want fast-cheap", d2.Model)
	}
	if r := discardReasonFor(d2, "frontier-pricey"); r != DiscardTierNotAllowed {
		t.Errorf("frontier must stay discarded in economy mode, got %q", r)
	}
}

// --- Golden 9: an unknown requested model is an ERROR, not a substitution ---
func TestBaseline_UnknownModel_NoSilentSubstitution(t *testing.T) {
	p := legacyPolicy()
	p.AutoCheapest = true
	if _, err := Decide(legacyCatalog(), p, nil, nil,
		RequestShape{InputTokens: 100, RequestedModel: "model-with-typo"}, baseNow); err != ErrUnknownModel {
		t.Errorf("err = %v, want ErrUnknownModel", err)
	}
}

// --- Golden 10: unavailability DEGRADES, it does not refuse ---------------
func TestBaseline_AllUnavailable_DegradesAndServes(t *testing.T) {
	p := legacyPolicy()
	p.AutoCheapest = true
	h := &Hints{Unavailable: map[string]time.Time{
		"frontier-pricey": baseNow.Add(time.Hour), "balanced-mid": baseNow.Add(time.Hour),
		"fast-cheap": baseNow.Add(time.Hour),
	}}
	d, err := Decide(legacyCatalog(), p, h, nil,
		RequestShape{InputTokens: 1000, MaxOutputTokens: 500}, baseNow)
	if err != nil {
		t.Fatalf("unavailability must not refuse the request: %v", err)
	}
	if !d.AvailabilityDegraded {
		t.Error("AvailabilityDegraded should be set")
	}
	if d.Model == "" {
		t.Error("some model must still be served when all are marked unavailable")
	}
}

// --- Golden 11: no identity declared means no identity reported -----------
//
// Precise lock on Property 1. The safety net caught an over-broad assertion here
// and forced the distinction, which is worth recording:
//
//   - The DECISION must be untouched: same model, same discards, same savings
//     reason. That is what "an existing config decides exactly as before" means.
//   - SwapClass MAY be populated without any model_id, because a tier drop is a
//     real downgrade regardless of identity — and reporting it is the feature
//     earning its keep even for customers who never declare identity.
//   - ServedModelID must stay empty: identity is never invented.
func TestBaseline_NoIdentityDeclared_ReportsNoIdentity(t *testing.T) {
	p := legacyPolicy()
	p.AutoCheapest = true
	d, err := Decide(legacyCatalog(), p, nil, nil,
		RequestShape{InputTokens: 1000, MaxOutputTokens: 500, RequestedModel: "frontier-pricey"}, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	// The decision itself is the golden.
	if d.Model != "fast-cheap" {
		t.Errorf("model = %q, want fast-cheap — the decision must not change", d.Model)
	}
	if d.ServedModelID != "" {
		t.Errorf("ServedModelID = %q; no route declared a model_id, so none may be reported", d.ServedModelID)
	}
	// Never same_model without a declaration: that is the claim we cannot make.
	if d.SwapClass == SwapSameModel {
		t.Error("same_model requires a declared identity; it must never be inferred")
	}
	// No swap policy declared, so nothing may be blocked by it.
	if r := discardReasonFor(d, "fast-cheap"); r == DiscardSwapNotAllowed {
		t.Error("with no swap policy declared, nothing may be discarded by swap policy")
	}
}
