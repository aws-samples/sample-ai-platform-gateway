// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import "testing"

// reasoningCatalog: one model that can think, one that cannot, priced so the NON-thinking one
// is cheaper. That ordering is the point — with auto-cheapest on, only the capability check
// can stop the cheap one from winning.
func reasoningCatalog() []Candidate {
	return []Candidate{
		{Model: "cheap-no-think", Provider: "bedrock",
			Caps:   Capabilities{ToolUse: true, Tier: "fast"},
			Prices: PriceHistory{{Standard: Layer{0.0001, 0.0001}}}},
		{Model: "thinker", Provider: "bedrock",
			Caps:   Capabilities{ToolUse: true, Tier: "frontier", Reasoning: true},
			Prices: PriceHistory{{Standard: Layer{0.01, 0.03}}}},
	}
}

func reasoningPol() Policy {
	return Policy{AutoCheapest: true, DefaultOutTok: 512, MinHintSamples: 20}
}

// A request that asks to think must be ROUTED to a model that can, even when a cheaper model
// is available. Same rule as tool use, and for the same reason: asking a model that cannot
// think to think is a provider rejection, not a graceful degradation.
func TestDecide_ReasoningRequestPicksACapableModel(t *testing.T) {
	req := RequestShape{InputTokens: 100, MaxOutputTokens: 4096, WantsReasoning: true, ThinkingBudget: 2048}
	d, err := Decide(reasoningCatalog(), reasoningPol(), nil, nil, req, now)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "thinker" {
		t.Errorf("model = %q, want thinker — cost must not beat a capability requirement", d.Model)
	}
	var found bool
	for _, dd := range d.Discards {
		if dd.Model == "cheap-no-think" && dd.Reason == DiscardNoReasoning {
			found = true
		}
	}
	if !found {
		t.Errorf("the incapable model must be discarded as %q, discards: %+v", DiscardNoReasoning, d.Discards)
	}
}

// Without the flag, nothing changes: the cheapest still wins. The capability is a
// requirement of the REQUEST, never a preference of the catalog.
func TestDecide_WithoutReasoningRequestTheCheapestStillWins(t *testing.T) {
	req := RequestShape{InputTokens: 100, MaxOutputTokens: 512}
	d, err := Decide(reasoningCatalog(), reasoningPol(), nil, nil, req, now)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Model != "cheap-no-think" {
		t.Errorf("model = %q, want the cheapest — reasoning capability must not attract traffic on its own", d.Model)
	}
}

// An absent capability counts as FALSE, exactly like ToolUse. A permissive default would turn
// a routable request into a provider error, and only for the customers who adopted the
// feature.
func TestDecide_AbsentReasoningCapabilityIsNotCapable(t *testing.T) {
	cands := []Candidate{{Model: "undeclared", Provider: "bedrock",
		Caps:   Capabilities{ToolUse: true, Tier: "frontier"}, // no Reasoning field set
		Prices: PriceHistory{{Standard: Layer{0.001, 0.001}}}}}
	req := RequestShape{InputTokens: 10, MaxOutputTokens: 2048, WantsReasoning: true, ThinkingBudget: 1024}
	if _, err := Decide(cands, reasoningPol(), nil, nil, req, now); err != ErrNoEligibleModel {
		t.Errorf("err = %v, want ErrNoEligibleModel — an undeclared capability must not be assumed", err)
	}
}

// Thinking tokens ARE output tokens: the provider counts and bills them there. A budget that
// does not enter the context-window check lets a request through that the provider then
// rejects, which reads to the customer as the gateway routing badly.
func TestIneligible_ThinkingBudgetCountsAgainstTheContextWindow(t *testing.T) {
	c := Candidate{Model: "narrow", Caps: Capabilities{Reasoning: true, ContextWindow: 10_000}}
	pol := Policy{DefaultOutTok: 512}

	// 8000 in + 1000 out fits comfortably on its own.
	fits := RequestShape{InputTokens: 8000, MaxOutputTokens: 1000, WantsReasoning: true}
	if r, ok := ineligible(c, pol, fits, Identity{}); !ok {
		t.Errorf("without a budget this must fit, discarded as %q", r)
	}
	// The same request asking for 4096 tokens of thinking no longer fits.
	overflows := fits
	overflows.ThinkingBudget = 4096
	r, ok := ineligible(c, pol, overflows, Identity{})
	if ok {
		t.Error("input + max_tokens + thinking budget exceeds the window; it must be discarded")
	}
	if r != DiscardContextTooSmall {
		t.Errorf("reason = %q, want %q", r, DiscardContextTooSmall)
	}
}
