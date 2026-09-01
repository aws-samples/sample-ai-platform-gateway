// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Bundle resolution and its interaction with eligibility (tasks 10 and 11).
package routing

import (
	"reflect"
	"testing"
)

func bundleCands() []Candidate {
	return []Candidate{
		{Model: "gpt-openai", Provider: "openai_compatible", ModelID: "openai/gpt-5.2",
			Caps: Capabilities{Tier: "frontier", ToolUse: true}, Prices: flatPrice(0.010, 0.030)},
		{Model: "gpt-azure", Provider: "azure", ModelID: "openai/gpt-5.2",
			Caps: Capabilities{Tier: "frontier", ToolUse: true}, Prices: flatPrice(0.005, 0.015)},
		{Model: "sonnet", Provider: "anthropic", ModelID: "anthropic/sonnet",
			Caps: Capabilities{Tier: "frontier", ToolUse: true}, Prices: flatPrice(0.003, 0.015)},
		{Model: "haiku", Provider: "anthropic", ModelID: "anthropic/haiku",
			Caps: Capabilities{Tier: "fast"}, Prices: flatPrice(0.0001, 0.0004)},
	}
}

// Layer order is the meaning. A model id expands to its whole group, which is what
// makes "prefer gpt-5.2 wherever it is served" survive adding a provider.
func TestResolveBundle_LayerOrderAndGroupExpansion(t *testing.T) {
	cands := bundleCands()
	id := BuildIdentity(cands)
	order, discards := ResolveBundle(Bundle{Layers: []BundleLayer{
		{Routes: []string{"openai/gpt-5.2"}},
		{Routes: []string{"sonnet"}},
		{Routes: []string{"haiku"}},
	}}, cands, id)

	want := []string{"gpt-openai", "gpt-azure", "sonnet", "haiku"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	if len(discards) != 0 {
		t.Errorf("unexpected discards: %+v", discards)
	}
}

// A typo must not take a production flow down: the reference is discarded, recorded,
// and the valid ones still serve.
func TestResolveBundle_BrokenReferenceDegrades(t *testing.T) {
	cands := bundleCands()
	order, discards := ResolveBundle(Bundle{Layers: []BundleLayer{
		{Routes: []string{"gpt-opnai", "gpt-azure"}}, // first is a typo
	}}, cands, BuildIdentity(cands))

	if !reflect.DeepEqual(order, []string{"gpt-azure"}) {
		t.Errorf("order = %v, want [gpt-azure]", order)
	}
	if len(discards) != 1 || discards[0].Model != "gpt-opnai" || discards[0].Reason != DiscardBundleRefUnknown {
		t.Errorf("discards = %+v, want one bundle_ref_unknown for gpt-opnai", discards)
	}
}

// Nothing valid at all falls back to the config's default order rather than
// refusing — ApplyBundle leaves ModelOrder untouched.
func TestApplyBundle_NoValidReferenceKeepsDefaultOrder(t *testing.T) {
	cands := bundleCands()
	pol := Policy{ModelOrder: []string{"sonnet", "haiku"},
		Bundle: &Bundle{Layers: []BundleLayer{{Routes: []string{"nope", "also-nope"}}}}}
	got, discards := ApplyBundle(pol, cands, BuildIdentity(cands))
	if !reflect.DeepEqual(got.ModelOrder, []string{"sonnet", "haiku"}) {
		t.Errorf("ModelOrder = %v, want the config order preserved", got.ModelOrder)
	}
	if len(discards) != 2 {
		t.Errorf("want 2 discards recorded, got %+v", discards)
	}
}

// The bundle's swap ceiling is a default, not an override: the feature's own
// declaration is more specific and wins, matching config precedence everywhere.
func TestApplyBundle_FeatureSwapWinsOverBundleSwap(t *testing.T) {
	cands := bundleCands()
	id := BuildIdentity(cands)
	b := &Bundle{Swap: SwapAllowDowngrade, Layers: []BundleLayer{{Routes: []string{"sonnet"}}}}

	filled, _ := ApplyBundle(Policy{Bundle: b}, cands, id)
	if filled.Swap != SwapAllowDowngrade {
		t.Errorf("Swap = %q, want the bundle ceiling when the feature declares none", filled.Swap)
	}
	declared, _ := ApplyBundle(Policy{Swap: SwapSameModelOnly, Bundle: b}, cands, id)
	if declared.Swap != SwapSameModelOnly {
		t.Errorf("Swap = %q, want the feature declaration to win", declared.Swap)
	}
}

// Property 10: preference never beats possibility. A tools request must not be
// attempted on a route with no tool use, even when the bundle puts it first.
func TestEligible_BundleNeverPromotesAnIncapableRoute(t *testing.T) {
	cands := bundleCands()
	pol := Policy{Bundle: &Bundle{Layers: []BundleLayer{
		{Routes: []string{"haiku"}}, // first layer, but cannot do tool use
		{Routes: []string{"sonnet"}},
	}}}
	got := Eligible(cands, pol, RequestShape{InputTokens: 100, HasTools: true})
	if got["haiku"] {
		t.Error("haiku declares no tool use and must not be attemptable for a tools request")
	}
	if !got["sonnet"] {
		t.Error("sonnet declares tool use and must remain attemptable")
	}
}

// Eligible has to answer to the swap ceiling too, otherwise the attempt chain
// would quietly retry on a model the policy forbids after the decision refused it.
func TestEligible_HonorsSwapCeiling(t *testing.T) {
	cands := bundleCands()
	pol := Policy{Swap: SwapSameModelOnly}
	got := Eligible(cands, pol, RequestShape{InputTokens: 100, RequestedModel: "gpt-openai"})
	if !got["gpt-azure"] {
		t.Error("same model on another provider must stay attemptable — that is the point of the policy")
	}
	if got["sonnet"] || got["haiku"] {
		t.Errorf("a different model must not be attemptable under same_model_only: %v", got)
	}
}

// With cost optimization off, the resolved bundle order decides which route serves
// when the client names none — the same authority model_order already had.
func TestDecide_BundleOrderDrivesChoiceWithoutAutoCheapest(t *testing.T) {
	cands := bundleCands()
	pol := Policy{DefaultOutTok: 256, Bundle: &Bundle{Layers: []BundleLayer{
		{Routes: []string{"sonnet"}},
		{Routes: []string{"openai/gpt-5.2"}},
	}}}
	dec, err := Decide(cands, pol, nil, nil, RequestShape{InputTokens: 100}, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.Model != "sonnet" {
		t.Errorf("model = %q, want sonnet (first bundle layer)", dec.Model)
	}
}

// A bundle does not turn into an allowlist. Restricting what may serve is the job
// of allowed_models and of the feature pin; duplicating that authority here would
// let the two drift.
func TestDecide_BundleDoesNotRestrictWhatMayServe(t *testing.T) {
	cands := bundleCands()
	pol := Policy{AutoCheapest: true, DefaultOutTok: 256,
		Bundle: &Bundle{Layers: []BundleLayer{{Routes: []string{"sonnet"}}}}}
	dec, err := Decide(cands, pol, nil, nil, RequestShape{InputTokens: 100}, baseNow)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.Model != "haiku" {
		t.Errorf("model = %q, want haiku: cost optimization still ranges over the whole catalog", dec.Model)
	}
}
