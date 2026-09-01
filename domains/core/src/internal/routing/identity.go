// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Model identity and swap classification — PURE DOMAIN.
//
// The problem this solves came from a real customer case: a reasoning flow tuned
// for one specific model loses depth when the gateway swaps the model, and
// re-tuning the prompt is expensive. The gateway has three paths that swap models
// (cost optimization, provider-failure fallback, budget degrade) and none of them
// could tell a harmless swap from a risky one.
//
// The missing primitive was identity. A route's NAME was its only identity, so two
// routes serving the very same model through different providers were, to the
// system, two unrelated models. Three defects followed:
//
//  1. Pinning quality killed safe failover. A pin matches route names, so pinning
//     the OpenAI route made the Azure route (same weights) ineligible — the
//     customer had to choose between protecting quality and having failover.
//  2. The ledger labeled opposite events the same way: "switched provider, same
//     model" (no quality risk) and "switched model" (real risk) were both
//     `fallback`, both counterfactual.
//  3. The most defensible saving in the product could not be expressed: serving
//     the SAME model through a cheaper provider is VERIFIED saving, and there was
//     no way to state that two routes are the same model.
package routing

// Swap classes: the semantics of substituting one route for another.
const (
	// SwapNone means the served route is the requested one — no substitution.
	SwapNone = ""
	// SwapSameModel: different route, same declared model. The model served is
	// unchanged, so there is no quality risk from the swap itself.
	SwapSameModel = "same_model"
	// SwapEquivalent: different model whose tier is not lower. Capability-wise it
	// fits the request; response depth may still differ.
	SwapEquivalent = "equivalent"
	// SwapDowngrade: different model of a lower tier. Quality loss is expected.
	SwapDowngrade = "downgrade"
)

// Swap policies a customer can declare per feature.
const (
	// SwapAllowAll is the absence of a policy: every class is permitted. This is
	// the default on purpose — no existing org changes behavior because of this
	// feature.
	SwapAllowAll = ""
	// SwapSameModelOnly permits provider failover but never a model change.
	SwapSameModelOnly = "same_model_only"
	// SwapAllowEquivalent permits same-model and equivalent, but no downgrade.
	SwapAllowEquivalent = "allow_equivalent"
	// SwapAllowDowngrade permits any class.
	SwapAllowDowngrade = "allow_downgrade"
)

// Identity maps declared model identities to the routes that serve them.
type Identity struct {
	byModel map[string][]string // model_id -> route names
	byRoute map[string]string   // route name -> model_id
	// routes holds EVERY route name in the catalog, including routes with no
	// declared identity. It exists for the ambiguity rule: a pin value that names
	// an existing route must be read as a route name, and deciding that requires
	// knowing all route names, not only the ones carrying an identity.
	routes map[string]bool
}

// BuildIdentity groups candidates by their declared ModelID.
//
// Two rules are enforced here, in the constructor, rather than at each call site
// so that no future caller can skip them:
//
//  1. A candidate with no ModelID is its own group of one. Identity is never
//     inferred from ProviderModelID, route name or capabilities. Inference would
//     be tempting (the same model often shares a provider model id across
//     providers) and would produce a silent false positive exactly where the
//     damage is worst: the customer believing they have quality-safe failover and
//     receiving a different model. "Unknown" resolves to "not the same".
//
//  2. An aggregator never joins a group, even when it declares a shared ModelID.
//     An aggregator routes internally to varying upstreams and may serve a
//     different version or quantization BETWEEN REQUESTS, which breaks the
//     premise invisibly. The whole promise of SwapSameModel — no quality risk —
//     rests on the model actually being the same.
func BuildIdentity(cands []Candidate) Identity {
	id := Identity{
		byModel: make(map[string][]string),
		byRoute: make(map[string]string, len(cands)),
		routes:  make(map[string]bool, len(cands)),
	}
	for _, c := range cands {
		id.routes[c.Model] = true
		if c.ModelID == "" || c.Aggregator {
			continue
		}
		id.byRoute[c.Model] = c.ModelID
		id.byModel[c.ModelID] = append(id.byModel[c.ModelID], c.Model)
	}
	return id
}

// IsRoute reports whether the name is a route in the catalog, with or without a
// declared identity.
func (i Identity) IsRoute(name string) bool { return i.routes[name] }

// GroupOf returns the route names sharing the identity of the given route.
// A route with no declared identity returns just itself.
func (i Identity) GroupOf(route string) []string {
	mid, ok := i.byRoute[route]
	if !ok {
		return []string{route}
	}
	return i.byModel[mid]
}

// RoutesFor returns the routes serving a given model id, or nil when unknown.
func (i Identity) RoutesFor(modelID string) []string { return i.byModel[modelID] }

// ModelIDOf returns the declared model id of a route, or "" when undeclared.
func (i Identity) ModelIDOf(route string) string { return i.byRoute[route] }

// Known reports whether the model id was declared by at least one route.
func (i Identity) Known(modelID string) bool { return len(i.byModel[modelID]) > 0 }

// SameModel reports whether two candidates serve the same declared model.
//
// Undeclared identity is never "the same", and an aggregator is never the same as
// anything else — both follow from BuildIdentity excluding them from the index.
func (i Identity) SameModel(a, b Candidate) bool {
	ma, oka := i.byRoute[a.Model]
	mb, okb := i.byRoute[b.Model]
	return oka && okb && ma == mb
}

// SwapClassOf classifies a substitution. PURE.
//
// Tier comparison uses TierRank, which places an undeclared tier in the middle.
// That is deliberate: with no declaration we do not assert a downgrade, only a
// difference — and SwapEquivalent already carries enough quality risk for the
// customer to decide on.
func (i Identity) SwapClassOf(requested, served Candidate) string {
	if requested.Model == served.Model {
		return SwapNone
	}
	if i.SameModel(requested, served) {
		return SwapSameModel
	}
	if TierRank(served.Caps.Tier) < TierRank(requested.Caps.Tier) {
		return SwapDowngrade
	}
	return SwapEquivalent
}

// swapRank orders the classes by how much quality risk they carry.
func swapRank(class string) int {
	switch class {
	case SwapNone:
		return 0
	case SwapSameModel:
		return 1
	case SwapEquivalent:
		return 2
	case SwapDowngrade:
		return 3
	}
	return 3 // unknown class is treated as the riskiest: never permit by accident
}

// swapCeiling is the highest class each policy permits.
func swapCeiling(policy string) int {
	switch policy {
	case SwapSameModelOnly:
		return 1
	case SwapAllowEquivalent:
		return 2
	case SwapAllowDowngrade:
		return 3
	}
	return 3 // SwapAllowAll and any unknown policy: current behavior, permit all
}

// SwapAllowed reports whether a class is permitted by a policy.
//
// Serving the requested route itself (SwapNone) is always permitted — a policy
// restricts substitutions, never the customer's own choice.
//
// An unknown policy value permits everything, matching how the rest of the config
// treats unrecognized values: an unreadable policy must not silently start
// refusing traffic.
func SwapAllowed(policy, class string) bool {
	return swapRank(class) <= swapCeiling(policy)
}
