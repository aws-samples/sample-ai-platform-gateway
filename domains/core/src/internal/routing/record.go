// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Usage_Record assembly — PURE DOMAIN (hexagonal-refactor, task 6).
//
// Absorbs decorateUsage/decorateSavings/decorateEscalation, which used to live in the
// shell (cmd/router). They are pure functions: they take the decision plus the numbers
// from the response and write the derived fields into the record, with no IO at all.
// The field NAMES and semantics are identical to the shell's (Req 12.2) — the move is
// validated by the golden files under cmd/router/testdata (the emitted record must not
// change).
//
// Why it stays a map instead of a typed struct: the Usage_Record is a wire contract
// with Observability, with fields that evolve by addition; the shell already treated it
// as a map and swapping in a struct would risk changing the serialization. The spec
// does not alter the schema.
package routing

// DecorateSavings splits the record's savings into the two ledger classes.
//
// verifiedPortion is the observable share (response cache or provider prompt cache);
// the rest is counterfactual (we served a model different from the one requested and
// compared against what the request would have cost). Keeping the two in their own
// fields lets the ROI view show verified savings separately from conditional savings.
func DecorateSavings(rec map[string]interface{}, total, verifiedPortion float64, reason string) {
	v, c := SplitSavings(total, verifiedPortion)
	rec["saved_verified_usd"] = v
	rec["saved_counterfactual_usd"] = c
	rec["savings_class"] = ClassOf(reason)
}

// DecorateUsage adds the fields the decision produced to the Usage_Record.
//
// `mode` is already emitted with the value sync even though batch does not exist yet:
// it is the seam that prevents a batch record (hours of latency) from contaminating P95
// and the SLI later. The cache counters come in as primitives (cacheRead/cacheWrite/
// cacheConv) so the domain does not depend on the shell's `result` type.
func DecorateUsage(rec map[string]interface{}, dec Decision, requested, pricingStatus string, cost float64, cacheRead, cacheWrite int, cacheConv string) {
	rec["mode"] = "sync"
	rec["requested_model"] = requested
	// requested_cost_usd is the counterfactual BASELINE: what the REQUESTED model would
	// have cost for the same tokens. Persisting it (instead of deriving it from
	// cost+saved) is what makes auto_cheapest savings auditable line by line — without
	// it, the record says how much was saved but not against what.
	rec["requested_cost_usd"] = dec.RequestedCostUSD
	rec["pricing_status"] = pricingStatus
	rec["out_tokens_source"] = dec.OutTokensSource
	rec["economy_mode"] = dec.EconomyMode
	rec["availability_degraded"] = dec.AvailabilityDegraded
	rec["cache_read_input_tokens"] = cacheRead
	rec["cache_write_input_tokens"] = cacheWrite
	if cacheConv != "" {
		rec["cache_counters"] = cacheConv
	}
	// paid_from separates credit consumption from real cash outlay: adding the two up in
	// the ledger would count the same quantity twice.
	rec["paid_from"] = dec.PaidFrom
	if dec.PaidFrom == PaidFromCredit {
		rec["credit_usd"], rec["cash_usd"] = cost, 0.0
	} else {
		rec["credit_usd"], rec["cash_usd"] = 0.0, cost
	}
}

// DecorateEscalation records the escalation in the Usage_Record.
func DecorateEscalation(rec map[string]interface{}, escalated bool, reason, outcome string, attempts int, requestedCost, totalCost float64) {
	rec["attempt_count"] = attempts
	rec["escalated"] = escalated
	if reason != "" {
		rec["escalation_reason"] = reason
	}
	if outcome != "" {
		rec["escalation_outcome"] = outcome
	}
	saved, floored := NetSavings(requestedCost, totalCost)
	rec["saved_net_usd"] = saved
	rec["savings_floored"] = floored
}
