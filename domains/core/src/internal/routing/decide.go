// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import "time"

// Decide picks the model in THREE layers, in this order (Req 1.5, 2, 10, 16.4):
//
//	(a) ELIGIBILITY — hard filter: allowed_models, feature policy, tool use,
//	    multimodal, context window. Emptying this one is a customer error.
//	(b) AVAILABILITY — reversible filter: quota exceeded, recent failure.
//	    Emptying this one is operational noise and the decision DEGRADES instead
//	    of refusing.
//	(c) EXPECTED COST — optimization among the survivors.
//
// The order is not cosmetic. Optimizing before filtering is exactly the bug that
// sent tool use to nova-micro: the cheapest won without knowing whether it was capable.
//
// Layer (a) applies ALWAYS, including with auto_cheapest turned off — a model's
// capability does not depend on cost optimization being active.
func Decide(
	cands []Candidate,
	pol Policy,
	hints *Hints,
	credit *CreditState,
	req RequestShape,
	now time.Time,
) (Decision, error) {
	d := Decision{
		EconomyMode:     pol.EconomyMode,
		OutTokensSource: OutSourceHeuristic,
		PricingStatus:   PricingUnknown,
		PaidFrom:        PaidFromCash,
	}

	// A requested model that does NOT exist in the catalog is a customer error, not
	// an invitation to substitute silently. Without this check, a typo in the model
	// name would make auto-cheapest serve another one and the customer would never
	// know they are running something different from what they asked for.
	if req.RequestedModel != "" {
		if _, ok := findCandidate(cands, req.RequestedModel); !ok {
			return d, ErrUnknownModel
		}
	}

	// Identity index: built once and threaded through eligibility, optimization and
	// the final classification. Absent model_id everywhere makes it inert, which is
	// what keeps a pre-feature config deciding exactly as before.
	id := BuildIdentity(cands)
	requested, hasRequested := findCandidate(cands, req.RequestedModel)

	// Bundle resolution happens HERE, before eligibility, so the resolved order is
	// preference-only and still has to pass every filter below it (Property 10).
	// Broken references are recorded as discards and the request goes on.
	var bundleDiscards []Discard
	pol, bundleDiscards = ApplyBundle(pol, cands, id)
	d.Discards = append(d.Discards, bundleDiscards...)

	// --- (a) eligibility -----------------------------------------------------
	eligible := make([]Candidate, 0, len(cands))
	swapBlocked := false
	for _, c := range cands {
		if r, ok := ineligible(c, pol, req, id); !ok {
			d.Discards = append(d.Discards, Discard{Model: c.Model, Reason: r})
			continue
		}
		// Swap policy lives in the eligibility layer so it binds every path that
		// could substitute a route: cost optimization, provider fallback and budget
		// degrade. Only checkable when we know what was requested.
		if hasRequested && !SwapAllowed(pol.Swap, id.SwapClassOf(requested, c)) {
			d.Discards = append(d.Discards, Discard{Model: c.Model, Reason: DiscardSwapNotAllowed})
			swapBlocked = true
			continue
		}
		eligible = append(eligible, c)
	}
	if len(eligible) == 0 {
		// Distinguish "policy did its job" from "catalog has nothing capable": the
		// customer's next action is different in each case.
		if swapBlocked {
			return d, ErrSwapNotAllowed
		}
		return d, ErrNoEligibleModel
	}

	// --- (b) availability ----------------------------------------------------
	available := make([]Candidate, 0, len(eligible))
	for _, c := range eligible {
		if r, ok := unavailable(c, hints, now); !ok {
			d.Discards = append(d.Discards, Discard{Model: c.Model, Reason: r})
			continue
		}
		available = append(available, c)
	}
	if len(available) == 0 {
		// Operational noise must not refuse the request (Req 10.5).
		available = eligible
		d.AvailabilityDegraded = true
	}

	// --- cost of the REQUESTED model (for the ledger, Req 2.8) ---------------
	// Computed before the optimization and regardless of whether it survived:
	// savings are measured against what the customer asked for, not against
	// whatever was left.
	if base, ok := findCandidate(cands, requestedOrDefault(req, pol, available)); ok {
		if tier, st := SelectPrice(base.Prices, now); st != PricingUnknown {
			eOut, _ := expectedOut(base.Model, pol, hints, req)
			d.RequestedCostUSD = ExpectedCost(tier, base.Caps.PerRequestFeeUSD,
				req.InputTokens, req.CachedInputTokens, eOut)
		}
	}

	// --- (c) expected cost ---------------------------------------------------
	// Without auto-cheapest we do not swap models for price: eligibility already
	// ran, and swapping here would contradict the customer's explicit choice.
	if !pol.AutoCheapest {
		chosen, ok := pickRequested(available, req, pol)
		if !ok {
			return d, ErrNoEligibleModel
		}
		return classify(finish(d, chosen, pol, hints, credit, req, now), requested, hasRequested, chosen, id), nil
	}

	type scored struct {
		c     Candidate
		gross Money
		cash  Money
		src   string
		st    string
	}
	var best *scored
	for _, c := range available {
		tier, st := SelectPrice(c.Prices, now)
		if st == PricingUnknown {
			// An unknown price leaves the OPTIMIZATION (Req 3.2). The model is still
			// servable if the customer asks for it — it just is not picked for being
			// "cheap".
			d.Discards = append(d.Discards, Discard{Model: c.Model, Reason: DiscardUnknownPrice})
			continue
		}
		eOut, src := expectedOut(c.Model, pol, hints, req)
		gross := ExpectedCost(tier, c.Caps.PerRequestFeeUSD, req.InputTokens, req.CachedInputTokens, eOut)
		cash, _ := CashCost(gross, c.Provider, credit, now)
		// Provider arbitrage needs NO special ordering here: routes of the same
		// declared model are ordinary candidates, so the cheapest already wins. What
		// the feature adds is naming the outcome (SwapSameModel) and classifying the
		// saving as VERIFIED instead of counterfactual.
		//
		// Deliberately NOT preferring the identity group over a cheaper different
		// model: that would quietly weaken the cost optimization the customer opted
		// into. Quality is governed by the swap policy, cost by auto-cheapest, and
		// the two compose — a customer who wants same-model only declares it.
		cur := scored{c: c, gross: gross, cash: cash, src: src, st: st}
		if best == nil || betterThan(cur.cash, cur.gross, cur.c.Model, best.cash, best.gross, best.c.Model, pol) {
			b := cur
			best = &b
		}
	}

	if best == nil {
		// Every eligible model lacks a usable price: serve the requested/default one
		// instead of refusing — refusing for lack of a price would be worse than
		// serving.
		chosen, ok := pickRequested(available, req, pol)
		if !ok {
			return d, ErrNoEligibleModel
		}
		return classify(finish(d, chosen, pol, hints, credit, req, now), requested, hasRequested, chosen, id), nil
	}

	d.Model = best.c.Model
	d.ExpectedCostUSD = best.gross
	d.CashCostUSD = best.cash
	d.OutTokensSource = best.src
	d.PricingStatus = best.st
	if best.cash == 0 && best.gross > 0 {
		d.PaidFrom = PaidFromCredit
	}
	if d.RequestedCostUSD == 0 {
		d.RequestedCostUSD = best.gross
	}
	return classify(d, requested, hasRequested, best.c, id), nil
}

// classify records the swap semantics on the decision.
//
// Both are empty when nothing was substituted or when no identity was declared,
// which is what keeps a pre-feature config producing no swap signal at all
// (Property 1). Only the served identity is recorded — the requested one is
// already implied by the requested model name.
func classify(d Decision, requested Candidate, hasRequested bool, served Candidate, id Identity) Decision {
	d.ServedModelID = id.ModelIDOf(served.Model)
	if hasRequested {
		d.SwapClass = id.SwapClassOf(requested, served)
	}
	return d
}

// ineligible applies layer (a). Returns (reason, false) when it discards.
func ineligible(c Candidate, pol Policy, req RequestShape, id Identity) (string, bool) {
	if !allowedIn(pol.AllowedModels, c.Model) {
		return DiscardNotAllowed, false
	}
	if !tierAllowed(c, pol, id) {
		return DiscardTierNotAllowed, false
	}
	// An absent capability counts as FALSE: better not to route than to return
	// `arguments: {}` from a model that cannot do tool use (Req 1.2).
	if req.HasTools && !c.Caps.ToolUse {
		return DiscardNoToolUse, false
	}
	if req.HasImage && !c.Caps.Multimodal {
		return DiscardNotMultimodal, false
	}
	// A ContextWindow of zero means UNKNOWN and does not discard (Req 1.4).
	//
	// When the client does not inform `max_tokens`, assuming ZERO output would
	// underestimate the need and let through a model that cannot fit the response.
	// The output default is used as a floor — the same quantity the cost estimator
	// uses.
	if c.Caps.ContextWindow > 0 {
		out := req.MaxOutputTokens
		if out <= 0 {
			out = pol.DefaultOutTok
			if out <= 0 {
				out = 512
			}
		}
		if req.InputTokens+out > c.Caps.ContextWindow {
			return DiscardContextTooSmall, false
		}
	}
	return "", true
}

// tierAllowed applies the feature's quality policy (Req 11.3, 12.2, 12.3).
// FeatureModels is more specific and, when present, wins.
func tierAllowed(c Candidate, pol Policy, id Identity) bool {
	if len(pol.FeatureModels) > 0 {
		return pinMatches(pol.FeatureModels, c, id)
	}
	if len(pol.FeatureTiers) == 0 {
		return true
	}
	if allowedIn(pol.FeatureTiers, c.Caps.Tier) {
		return true
	}
	if !pol.EconomyMode {
		return false
	}
	// Economy mode allows a tier LOWER than the declared one, not any tier (Req 12.2).
	// The distinction matters: allowing a higher tier would turn "I accept a worse
	// response to spend less" into "you may spend more", which is the opposite of
	// what was asked.
	max := 0
	for _, t := range pol.FeatureTiers {
		if r := TierRank(t); r > max {
			max = r
		}
	}
	return TierRank(c.Caps.Tier) <= max
}

// pinMatches decides whether a candidate satisfies a pin list whose entries may
// be either a route name or a declared model id.
//
// This is what fixes the sharpest flaw of the pre-feature behavior: pinning by
// route name protected quality but made the same model on another provider
// ineligible, forcing the customer to choose between protecting quality and having
// failover. Pinning by model id now enables the whole identity group, so both hold
// at once.
//
// Precedence when a value is ambiguous (a route literally named like a model id):
// ROUTE NAME wins. A route name points at exactly one route while a model id opens
// a set, so when in doubt we restrict.
func pinMatches(pins []string, c Candidate, id Identity) bool {
	if len(pins) == 0 {
		return true
	}
	for _, p := range pins {
		if p == c.Model {
			return true
		}
	}
	for _, p := range pins {
		// A pin naming an existing route was already resolved above; reading it as a
		// model id here would widen a pin the customer meant to narrow.
		if id.IsRoute(p) {
			continue
		}
		if id.ModelIDOf(c.Model) == p {
			return true
		}
	}
	return false
}

// unavailable applies layer (b) using the signal published in the hints (Req 10.2/10.3).
func unavailable(c Candidate, hints *Hints, now time.Time) (string, bool) {
	if hints == nil || len(hints.Unavailable) == 0 {
		return "", true
	}
	if until, ok := hints.Unavailable[c.Model]; ok && now.Before(until) {
		return DiscardProviderQuota, false
	}
	if until, ok := hints.Unavailable[c.Provider]; ok && now.Before(until) {
		return DiscardProviderRecentFailure, false
	}
	return "", true
}

// expectedOut resolves E[tokens_out] by the precedence of Req 9.6, also returning
// the source so it can be recorded in the Usage_Record (Req 9.9).
func expectedOut(model string, pol Policy, hints *Hints, req RequestShape) (int, string) {
	if hints != nil {
		// Precedence from the most specific to the most general, evaluating the
		// sample threshold ON EACH key: the feature may not have enough history
		// while the org aggregate does, and in that case the aggregate is better
		// than the heuristic.
		if req.Feature != "" {
			if v, ok := hints.medianFor(req.Feature+"|"+model, pol.MinHintSamples); ok {
				return v, OutSourceHintOrgFeatureModel
			}
		}
		if v, ok := hints.medianFor("*|"+model, pol.MinHintSamples); ok {
			return v, OutSourceHintOrgModel
		}
		if v, ok := hints.medianFor(model, pol.MinHintSamples); ok {
			return v, OutSourceHintModel
		}
	}
	return heuristicOut(pol, req), OutSourceHeuristic
}

// heuristicOut is the fallback of Req 2.3: the smaller of the requested ceiling and
// the environment default. A fallback has to exist — hints being unavailable must
// not take the decision down nor bias it toward zero (zero would make every model
// look free).
func heuristicOut(pol Policy, req RequestShape) int {
	def := pol.DefaultOutTok
	if def <= 0 {
		def = 512
	}
	if req.MaxOutputTokens > 0 && req.MaxOutputTokens < def {
		return req.MaxOutputTokens
	}
	return def
}

// betterThan orders by CASH, breaks ties by GROSS and then by model_order
// (Req 2.5, 16.2). Ordering by cash is what makes credit preferred without
// touching the price per token.
func betterThan(cashA, grossA Money, modelA string, cashB, grossB Money, modelB string, pol Policy) bool {
	if cashA != cashB {
		return cashA < cashB
	}
	if grossA != grossB {
		return grossA < grossB
	}
	ia, ib := orderIndex(pol.ModelOrder, modelA), orderIndex(pol.ModelOrder, modelB)
	return ia < ib
}

func orderIndex(order []string, model string) int {
	for i, m := range order {
		if m == model {
			return i
		}
	}
	return len(order) // outside the order goes to the end
}

func findCandidate(cands []Candidate, model string) (Candidate, bool) {
	for _, c := range cands {
		if c.Model == model {
			return c, true
		}
	}
	return Candidate{}, false
}

// requestedOrDefault returns the reference model for the ledger: the one the client
// asked for, otherwise the first in the declared order, otherwise the first
// available one.
func requestedOrDefault(req RequestShape, pol Policy, available []Candidate) string {
	if req.RequestedModel != "" {
		return req.RequestedModel
	}
	for _, m := range pol.ModelOrder {
		if _, ok := findCandidate(available, m); ok {
			return m
		}
	}
	if len(available) > 0 {
		return available[0].Model
	}
	return ""
}

// pickRequested chooses without optimizing: the requested model if it survived the
// filters, otherwise the first in the declared order.
func pickRequested(available []Candidate, req RequestShape, pol Policy) (Candidate, bool) {
	if req.RequestedModel != "" {
		if c, ok := findCandidate(available, req.RequestedModel); ok {
			return c, true
		}
	}
	for _, m := range pol.ModelOrder {
		if c, ok := findCandidate(available, m); ok {
			return c, true
		}
	}
	if len(available) > 0 {
		return available[0], true
	}
	return Candidate{}, false
}

// finish fills in the decision for the path without cost optimization.
func finish(d Decision, c Candidate, pol Policy, hints *Hints, credit *CreditState,
	req RequestShape, now time.Time) Decision {
	d.Model = c.Model
	tier, st := SelectPrice(c.Prices, now)
	d.PricingStatus = st
	eOut, src := expectedOut(c.Model, pol, hints, req)
	d.OutTokensSource = src
	if st != PricingUnknown {
		d.ExpectedCostUSD = ExpectedCost(tier, c.Caps.PerRequestFeeUSD,
			req.InputTokens, req.CachedInputTokens, eOut)
		d.CashCostUSD, d.PaidFrom = CashCost(d.ExpectedCostUSD, c.Provider, credit, now)
	}
	if d.RequestedCostUSD == 0 {
		d.RequestedCostUSD = d.ExpectedCostUSD
	}
	return d
}
