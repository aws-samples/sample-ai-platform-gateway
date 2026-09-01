// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

// Classification of the savings recorded in the ROI ledger.
//
// Why separate them: the two kinds of savings the gateway produces have VERY different
// strength of proof, and adding them into a single number makes the ledger indefensible
// at the customer's first hard question.
//
//   - VERIFIED — the same model cost less, and that is observable without assuming
//     anything. Response cache (there was no provider call) and provider prompt cache
//     (the provider itself reported fewer billed tokens). There is no counterfactual:
//     the model served is the model that would be served.
//
//   - COUNTERFACTUAL — we served a model DIFFERENT from the one requested and compared
//     against what the request would have cost. The money saved is real, but the
//     baseline is an assumption: it only holds if the customer would in fact have run
//     the requested model. When the requested name is merely a generic route alias, the
//     baseline represents no choice at all and the "savings" measures very little.
//
// Product consequence: gain-share must rest on VERIFIED savings. The counterfactual one
// is informative and depends on the baseline being a commitment from the customer
// (per-feature policy), not an alias they wrote out of habit.
const (
	SavingsVerified       = "verified"
	SavingsCounterfactual = "counterfactual"
)

// Savings reasons emitted by the gateway.
const (
	ReasonResponseCache       = "cache"                 // identical response reused
	ReasonProviderPromptCache = "provider_prompt_cache" // provider charged less for the prefix
	ReasonSemanticCache       = "semantic_cache"        // response to a SEMANTICALLY close question (approximate)
	ReasonAutoCheapest        = "auto_cheapest"         // cheaper equivalent model
	ReasonFallback            = "fallback"              // alternative provider/model
	ReasonBudgetDegrade       = "budget_degrade"        // degradation forced by budget
	// ReasonProviderArbitrage: the SAME declared model served through a cheaper
	// provider. The only cost reduction in the product with no quality tradeoff.
	ReasonProviderArbitrage = "provider_arbitrage"
)

// ClassOf classifies a savings reason. An unknown reason is treated as COUNTERFACTUAL on
// purpose: when in doubt, the savings is the least defensible one, never the most.
// Classifying upward by mistake inflates the number that backs the invoice.
func ClassOf(reason string) string {
	switch reason {
	// Provider arbitrage joins the verified column by the SAME criterion already
	// stated above — "the model served is the model that would be served". Same
	// declared model, lower price, no counterfactual baseline to assume.
	//
	// One honesty caveat that must reach the UI: we verify that the PRICE was
	// lower, not that the weights are identical. Identity is the customer's
	// declaration, and providers of the same model can diverge by version, region,
	// moderation layer or quantization.
	case ReasonResponseCache, ReasonProviderPromptCache, ReasonProviderArbitrage:
		return SavingsVerified
	case "":
		return ""
	default:
		return SavingsCounterfactual
	}
}

// SplitSavings splits the total savings into the two classes, given the share that is
// verified (cache). Floor of zero on both parts and the invariant preserved:
//
//	verified + counterfactual == total    (when total >= 0)
//
// An inconsistent provider may report a cache share larger than the total; in that case
// the verified part is capped at the total and the counterfactual goes to zero, instead
// of going negative and "discounting" savings that did happen.
func SplitSavings(total, verifiedPortion float64) (verified, counterfactual float64) {
	if total <= 0 {
		return 0, 0
	}
	if verifiedPortion < 0 {
		verifiedPortion = 0
	}
	if verifiedPortion > total {
		verifiedPortion = total
	}
	return verifiedPortion, total - verifiedPortion
}
