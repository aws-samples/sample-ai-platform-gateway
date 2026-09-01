// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Fallback bundles — PURE DOMAIN.
//
// A bundle is the declarative packaging of "try these, in this order". It exists
// because the ordering a customer actually wants for a critical flow is layered by
// INTENT, not flat by price: first the same model wherever it is served, then a
// model of the same tier, and only then something cheaper. Expressing that with
// model_order alone forced them to hand-maintain one flat list per feature and to
// re-edit it every time a provider was added.
//
// The line this file must not cross is stated in Decision 6 of the design: a
// bundle is PREFERENCE, never CAPABILITY. It reorders attempts among routes that
// are already allowed to serve; it can never promote a route into serving a
// request it cannot handle. That inversion is what once sent tool-use traffic to a
// model with no tool use.
package routing

// BundleLayer is one tier of preference. Entries are route names OR declared model
// ids; a model id expands to every route serving it, which is the whole point —
// "prefer gpt-5.2 wherever it lives" survives adding a provider tomorrow.
type BundleLayer struct {
	Routes []string `json:"routes"`
}

// Bundle is an ordered set of layers plus the swap ceiling that applies to all of
// them. Carrying the policy here means the customer declares intent once, next to
// the order it governs, instead of remembering to repeat it per feature.
type Bundle struct {
	Name   string        `json:"name,omitempty"`
	Layers []BundleLayer `json:"layers"`
	Swap   string        `json:"swap,omitempty"`
}

// ResolveBundle flattens the layers into a single attempt order.
//
// Rules, all of them consequences of "degrade, never refuse":
//
//   - Layer order is preserved, and within a layer the declared order is kept.
//     Nothing is sorted: the customer's sequence IS the meaning.
//   - A model id expands to its identity group. A route with no declared identity
//     is reached only by its own name.
//   - A reference to something that does not exist is DISCARDED and recorded, not
//     an error. A typo in a bundle must not take a production flow down; the
//     remaining valid references still serve, and the discard makes the mistake
//     visible in the decision log.
//   - Duplicates collapse to their first occurrence, so a route named directly in
//     layer 1 is not demoted by also appearing inside a group in layer 2.
func ResolveBundle(b Bundle, cands []Candidate, id Identity) (order []string, discards []Discard) {
	seen := make(map[string]bool, len(cands))
	for _, layer := range b.Layers {
		for _, ref := range layer.Routes {
			if ref == "" {
				continue
			}
			// Route name has precedence over model id, matching pinMatches: a name
			// points at exactly one route, so when a value could be read both ways we
			// take the narrower reading.
			if id.IsRoute(ref) {
				if !seen[ref] {
					seen[ref] = true
					order = append(order, ref)
				}
				continue
			}
			routes := id.RoutesFor(ref)
			if len(routes) == 0 {
				discards = append(discards, Discard{Model: ref, Reason: DiscardBundleRefUnknown})
				continue
			}
			for _, r := range routes {
				if !seen[r] {
					seen[r] = true
					order = append(order, r)
				}
			}
		}
	}
	return order, discards
}

// ApplyBundle folds the bundle into the policy that drives the decision.
//
// Two deliberate restraints:
//
//  1. The resolved order becomes ModelOrder — it does NOT become an allowlist.
//     Restricting WHICH models may serve is already the job of allowed_models and
//     of the feature pin; a bundle that also restricted would duplicate that
//     authority in a second place, and the two would drift.
//
//  2. The bundle's swap ceiling applies only when the feature declares none. The
//     more specific declaration wins, which is the same precedence the whole
//     config uses (app over team over org).
//
// A bundle that resolves to nothing leaves the policy untouched, so the config's
// default order still serves. Refusing traffic because a bundle was misspelled
// would be the worst possible trade.
func ApplyBundle(pol Policy, cands []Candidate, id Identity) (Policy, []Discard) {
	if pol.Bundle == nil {
		return pol, nil
	}
	order, discards := ResolveBundle(*pol.Bundle, cands, id)
	if len(order) > 0 {
		pol.ModelOrder = order
	}
	if pol.Swap == "" {
		pol.Swap = pol.Bundle.Swap
	}
	return pol, discards
}

// Eligible returns the set of route names that may be ATTEMPTED for this request.
//
// It exists because the decision and the attempt chain were two different
// authorities. Decide filters by capability; the fallback chain in the shell
// filtered only by allowed_models. A request carrying tools could therefore be
// RETRIED on a route unable to do tool use — the very defect the eligibility layer
// was built to kill, surviving one layer below it. Exporting the same predicate
// lets the chain builder answer to the same authority instead of a weaker copy.
func Eligible(cands []Candidate, pol Policy, req RequestShape) map[string]bool {
	id := BuildIdentity(cands)
	pol, _ = ApplyBundle(pol, cands, id)
	requested, hasRequested := findCandidate(cands, req.RequestedModel)
	out := make(map[string]bool, len(cands))
	for _, c := range cands {
		if _, ok := ineligible(c, pol, req, id); !ok {
			continue
		}
		if hasRequested && !SwapAllowed(pol.Swap, id.SwapClassOf(requested, c)) {
			continue
		}
		out[c.Model] = true
	}
	return out
}

// BundleOrder is the attempt order the shell should use when building the fallback
// chain, or nil when no bundle applies. Kept separate from ApplyBundle so the shell
// does not have to carry a second copy of the policy around.
func BundleOrder(pol Policy, cands []Candidate) []string {
	if pol.Bundle == nil {
		return nil
	}
	order, _ := ResolveBundle(*pol.Bundle, cands, BuildIdentity(cands))
	return order
}
