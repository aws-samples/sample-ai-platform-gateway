// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"encoding/json"
	"testing"

	"github.com/aiplat/core/internal/ports"
	"github.com/aiplat/core/internal/routing"
)

func ip(v int) *int { return &v }

// The effort→budget mapping lives in ONE place. Four adapters resolving "high"
// independently would be four different products under one name, and a customer comparing
// two providers would be measuring our inconsistency instead of the models.
func TestResolveReasoning_EffortMapsToABudget(t *testing.T) {
	cases := []struct {
		effort string
		want   int
	}{
		{"low", thinkingBudgetLow},
		{"medium", thinkingBudgetMedium},
		{"high", thinkingBudgetHigh},
		// Case and surrounding whitespace are the client's, not a different request.
		{"  HIGH  ", thinkingBudgetHigh},
	}
	for _, tc := range cases {
		p := resolveReasoning(tc.effort, nil, maxThinkingHardCap)
		if p.Req == nil || p.Req.BudgetTokens != tc.want {
			t.Errorf("effort %q resolved to %+v, want budget %d", tc.effort, p.Req, tc.want)
		}
		if p.Clamped != "" {
			t.Errorf("effort %q was clamped (%s) with the ceiling wide open", tc.effort, p.Clamped)
		}
	}
	// The smallest level has to be a budget every provider accepts: Anthropic rejects
	// anything below 1024, so a "low" that worked on Gemini and failed on Claude would be
	// the worst kind of portable-looking parameter.
	if thinkingBudgetLow != 1024 {
		t.Errorf("low = %d; Anthropic's documented minimum is 1024", thinkingBudgetLow)
	}
}

// No effort means no thinking request at all — not a request for zero thinking.
func TestResolveReasoning_AbsentIsNotARequest(t *testing.T) {
	p := resolveReasoning("", ip(500), maxThinkingHardCap)
	if p.Req != nil {
		t.Errorf("an absent effort must produce no reasoning request, got %+v", p.Req)
	}
	if p.MaxTokens != 500 {
		t.Errorf("max_tokens must pass through untouched, got %d", p.MaxTokens)
	}
}

// "none" carries information no absent field does, and the providers that can honour it are
// told so. Collapsing it into "unspecified" would silently let a model think when the client
// explicitly asked it not to.
func TestResolveReasoning_NoneIsAnExplicitDisable(t *testing.T) {
	p := resolveReasoning("none", nil, maxThinkingHardCap)
	if p.Req == nil || !p.Req.Disabled {
		t.Fatalf("none must produce an explicit disable, got %+v", p.Req)
	}
	if p.Req.BudgetTokens != 0 {
		t.Errorf("a disabled request must carry no budget, got %d", p.Req.BudgetTokens)
	}
}

// The operator's ceiling wins over the client's effort, and the reduction is RECORDED.
// Clamping silently would be the dishonest half of this design: the customer asked for high
// effort, paid for less, and nothing said so.
func TestResolveReasoning_OperatorCeilingClampsAndIsRecorded(t *testing.T) {
	p := resolveReasoning("high", nil, 2048)
	if p.Req.BudgetTokens != 2048 {
		t.Errorf("budget = %d, want it clamped to the ceiling 2048", p.Req.BudgetTokens)
	}
	if p.Clamped != "operator_ceiling" {
		t.Errorf("clamped = %q, want operator_ceiling — a silent clamp is unauditable", p.Clamped)
	}
}

// An explicit max_tokens has to leave room for an ANSWER. Satisfying Anthropic's
// max_tokens > budget_tokens by a single token is technically valid and produces a response
// that is all thinking and no answer.
func TestResolveReasoning_LeavesHeadroomForTheAnswer(t *testing.T) {
	p := resolveReasoning("high", ip(4096), maxThinkingHardCap)
	if p.MaxTokens != 4096 {
		t.Errorf("the client's max_tokens must not be raised, got %d", p.MaxTokens)
	}
	if want := 4096 - minAnswerHeadroom; p.Req.BudgetTokens != want {
		t.Errorf("budget = %d, want %d (max_tokens minus answer headroom)", p.Req.BudgetTokens, want)
	}
	if p.Clamped != "max_tokens_headroom" {
		t.Errorf("clamped = %q, want max_tokens_headroom", p.Clamped)
	}
	if p.Req.BudgetTokens >= p.MaxTokens {
		t.Error("the provider invariant max_tokens > budget_tokens is violated")
	}
}

// A max_tokens too small for the smallest valid budget drops thinking rather than sending a
// value the provider will reject: a served answer with a recorded reason beats a 400.
func TestResolveReasoning_MaxTokensTooSmallDropsThinking(t *testing.T) {
	p := resolveReasoning("high", ip(600), maxThinkingHardCap)
	if !p.Req.Disabled || p.Req.BudgetTokens != 0 {
		t.Errorf("thinking must be dropped, got %+v", p.Req)
	}
	if p.Clamped != "max_tokens_too_small" {
		t.Errorf("clamped = %q, want max_tokens_too_small", p.Clamped)
	}
}

// With no client ceiling, ours is raised to fit the thinking PLUS an answer. Leaving the
// provider default in place would let the chain of thought consume the whole allowance and
// return an empty answer — the request succeeds and the customer gets nothing.
func TestResolveReasoning_RaisesMaxTokensToFitThinkingPlusAnswer(t *testing.T) {
	p := resolveReasoning("medium", nil, maxThinkingHardCap)
	if p.MaxTokens != thinkingBudgetMedium+defaultOutTokens {
		t.Errorf("max_tokens = %d, want budget+%d", p.MaxTokens, defaultOutTokens)
	}
	if p.MaxTokens <= p.Req.BudgetTokens {
		t.Error("the raised ceiling must still leave room for the answer")
	}
}

// A typo is not treated as "no thinking": the client clearly meant to ask. Medium is the
// safe reading and the substitution is recorded, so the typo is visible in the ledger
// instead of costing nothing and doing nothing.
func TestResolveReasoning_UnknownEffortIsRecordedNotIgnored(t *testing.T) {
	p := resolveReasoning("maximum", nil, maxThinkingHardCap)
	if p.Req == nil || p.Req.BudgetTokens != thinkingBudgetMedium {
		t.Fatalf("an unknown effort must fall back to medium, got %+v", p.Req)
	}
	if p.Clamped != "unknown_effort" {
		t.Errorf("clamped = %q, want unknown_effort", p.Clamped)
	}
}

// The ceiling is bounded by a constant no configuration can raise. The scope chain REPLACES
// scalars (the same way budget and rate_limits behave), so a nested scope can set a larger
// number — a convention that lets a value grow needs one bound that does not move.
func TestEffectiveThinkingCeiling(t *testing.T) {
	if got := effectiveThinkingCeiling(&Config{}); got != defaultMaxThinkingTokens {
		t.Errorf("unset ceiling = %d, want the default %d", got, defaultMaxThinkingTokens)
	}
	if got := effectiveThinkingCeiling(&Config{MaxThinkingTokens: 999999}); got != maxThinkingHardCap {
		t.Errorf("ceiling = %d, want it bounded by the hard cap %d", got, maxThinkingHardCap)
	}
	if got := effectiveThinkingCeiling(&Config{MaxThinkingTokens: 4096}); got != 4096 {
		t.Errorf("ceiling = %d, want the configured 4096", got)
	}
	// An unbounded default would be the wrong way to fail: thinking is billed as output
	// tokens, so it would let one request cost several times what it cost yesterday with
	// nothing in the config to point at.
	if defaultMaxThinkingTokens <= 0 || defaultMaxThinkingTokens > maxThinkingHardCap {
		t.Errorf("the default ceiling (%d) must be positive and within the hard cap", defaultMaxThinkingTokens)
	}
}

// ── the ledger ────────────────────────────────────────────────────────────────

func TestDecorateReasoningRequest(t *testing.T) {
	m := map[string]interface{}{}
	decorateReasoningRequest(m, resolveReasoning("", nil, maxThinkingHardCap))
	if len(m) != 0 {
		t.Errorf("a request that asked for nothing must add nothing to the record: %v", m)
	}

	m = map[string]interface{}{}
	decorateReasoningRequest(m, resolveReasoning("high", nil, 2048))
	// Effort and budget are two fields because they can legitimately disagree, and that
	// disagreement is exactly what an operator needs to see when a ceiling is doing its job.
	if m["reasoning_effort"] != "high" || m["thinking_budget_tokens"] != 2048 {
		t.Errorf("record = %v, want the requested effort AND the budget actually sent", m)
	}
	if m["reasoning_clamped"] != "operator_ceiling" {
		t.Errorf("reasoning_clamped = %v; without it a policy reduction looks like the model choosing to think less", m["reasoning_clamped"])
	}
}

// ── the cache key ─────────────────────────────────────────────────────────────
//
// A parameter that changes the answer and not the key makes the cache swap semantics. For
// reasoning the consequence is sharper than for temperature: the first caller to ask without
// thinking would poison the entry for everyone who paid for thinking.
func TestCacheKey_ReasoningAndSamplingParamsPartition(t *testing.T) {
	base := routing.KeyInput{Org: "o", Model: "m", Messages: []ports.Message{{Role: "user", Text: "q"}}}
	k0 := routing.CacheKey(base, routing.KeyExact)

	variants := map[string]routing.KeyInput{
		"top_p":            func() routing.KeyInput { v := base; v.TopP = fp(0.5); return v }(),
		"stop":             func() routing.KeyInput { v := base; v.Stop = []string{"X"}; return v }(),
		"reasoning_effort": func() routing.KeyInput { v := base; v.ReasoningEffort = "high"; return v }(),
	}
	seen := map[string]string{k0: "no params"}
	for name, in := range variants {
		k := routing.CacheKey(in, routing.KeyExact)
		if prev, dup := seen[k]; dup {
			t.Errorf("%s produced the same key as %q — the cache would serve the wrong answer", name, prev)
		}
		seen[k] = name
	}
	// Two DIFFERENT efforts must not share a slot either.
	hi := base
	hi.ReasoningEffort = "high"
	lo := base
	lo.ReasoningEffort = "low"
	if routing.CacheKey(hi, routing.KeyExact) == routing.CacheKey(lo, routing.KeyExact) {
		t.Error("high and low effort must not share a cache entry")
	}
}

// A request sending none of the new parameters must hash to what it hashed BEFORE they
// existed, or every deployment loses its warm cache to this change. omitempty on the key
// material is what guarantees it, and this test is what keeps someone from removing it.
func TestCacheKey_UnusedParamsDoNotChangeExistingKeys(t *testing.T) {
	in := routing.KeyInput{Org: "o", Model: "m", Messages: []ports.Message{{Role: "user", Text: "q"}}}
	// The material is not exported, so the guarantee is checked the only way available from
	// outside: the key must be stable, and adding an explicitly-empty value must not move it.
	k1 := routing.CacheKey(in, routing.KeyExact)
	in.Stop = []string{}
	in.ReasoningEffort = ""
	if k2 := routing.CacheKey(in, routing.KeyExact); k1 != k2 {
		t.Errorf("an empty stop list / empty effort changed the key (%s vs %s); existing cache entries would be orphaned", k1, k2)
	}
}

// ── the request contract ──────────────────────────────────────────────────────

// The OpenAI dialect allows `stop` as a string OR an array. Rejecting the scalar would break
// a client that is correct against the spec we claim to speak.
func TestStopSequences_AcceptsBothDialectForms(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{`"END"`, []string{"END"}},
		{`["A","B"]`, []string{"A", "B"}},
		{`""`, nil},
		{`[]`, nil},
		// An empty stop sequence matches immediately on some providers, truncating the answer
		// to nothing — dropping it is the only safe reading.
		{`["A","","B"]`, []string{"A", "B"}},
		// Malformed is ignored, not fatal: a stop sequence is a refinement of the request and
		// refusing the whole call over it would be the worse trade.
		{`{"bad":1}`, nil},
		{`5`, nil},
	}
	for _, tc := range cases {
		got := stopSequences(json.RawMessage(tc.raw))
		if len(got) != len(tc.want) {
			t.Errorf("stop %s -> %v, want %v", tc.raw, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("stop %s -> %v, want %v", tc.raw, got, tc.want)
				break
			}
		}
	}
	if stopSequences(nil) != nil {
		t.Error("an absent stop must stay nil")
	}
}
