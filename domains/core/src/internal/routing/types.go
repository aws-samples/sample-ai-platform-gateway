// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package routing is the PURE DOMAIN of the gateway's routing decision.
//
// Boundary rule, verified by boundary_test.go: nothing here does IO. No SDK, no
// network, no files, no environment variables and no reading the clock — the
// reference instant comes in as a parameter. That is what makes price validity and
// credit expiration testable, and what lets us catch logic bugs without the cloud.
package routing

import (
	"errors"
	"time"
)

// Money in USD. float64 is acceptable because these values are a DECISION ESTIMATE,
// not an accounting entry; the realized cost that goes to the ledger is already
// float64 today. Switching to decimal is a separate decision.
type Money = float64

// Source of E[tokens_out], recorded in the Usage_Record (Req 9.9).
const (
	OutSourceHintOrgFeatureModel = "hint_org_feature_model"
	OutSourceHintOrgModel        = "hint_org_model"
	OutSourceHintModel           = "hint_model"
	OutSourceHeuristic           = "heuristic"
)

// Status of the applied price (Req 3.3, 5.6).
const (
	PricingKnown   = "known"
	PricingDerived = "derived"
	PricingUnknown = "unknown"
)

// Payment source (Req 15.11).
const (
	PaidFromCredit = "credit"
	PaidFromCash   = "cash"
)

// Reasons for discarding a candidate (Req 8.1).
const (
	DiscardNoToolUse             = "no_tool_use"
	DiscardNotMultimodal         = "not_multimodal"
	DiscardContextTooSmall       = "context_too_small"
	DiscardNotAllowed            = "not_allowed"
	DiscardTierNotAllowed        = "tier_not_allowed"
	DiscardUnknownPrice          = "unknown_price"
	DiscardProviderQuota         = "provider_quota"
	DiscardProviderRecentFailure = "provider_recent_failure"
	// DiscardSwapNotAllowed: the candidate would require a substitution of a class
	// the feature's swap policy does not permit.
	DiscardSwapNotAllowed = "swap_not_allowed"
	// DiscardBundleRefUnknown: a bundle layer referenced a route or model id that
	// does not exist in the catalog. Recorded rather than fatal — a typo in a
	// bundle must degrade the order, never refuse the request.
	DiscardBundleRefUnknown = "bundle_ref_unknown"
)

// ErrSwapNotAllowed: every capable candidate would require a substitution the
// declared swap policy forbids.
//
// Deliberately distinct from ErrNoEligibleModel: that one means the customer's
// catalog has no model able to serve the request (a configuration mistake); this
// one means the gateway is ENFORCING the policy the customer asked for. Sharing a
// code would leave the customer unable to tell "I configured it wrong" from "the
// gateway protected me".
var ErrSwapNotAllowed = errors.New("no route allowed by the swap policy")

// ErrNoEligibleModel: no candidate survived eligibility (Req 1.8).
// The handler translates it to HTTP 400 with code `no_eligible_model`.
var ErrNoEligibleModel = errors.New("no eligible model for this request")

// ErrUnknownModel: the client asked for a model that is not in the scope's catalog.
//
// It exists separately from ErrNoEligibleModel because the customer's action is
// different: here they fix the name; there they adjust the policy or the catalog.
// Silently substituting the model would hide a typo that changes what runs in
// production.
var ErrUnknownModel = errors.New("unknown model")

// Capabilities are a model's declared attributes in the catalog (Req 1.1).
//
// The defaults are conservative on purpose: no declaration means incapable of tool
// use and of multimodal. We would rather stop routing automatically than repeat the
// bug of sending tool use to a model that returns `arguments: {}`.
// A ContextWindow of zero means UNKNOWN and does not discard (Req 1.4).
type Capabilities struct {
	ToolUse       bool   `json:"tool_use"`
	Multimodal    bool   `json:"multimodal"`
	ContextWindow int    `json:"context_window_tokens"`
	Tier          string `json:"tier"`
	// PerRequestFeeUSD enters as the fourth component of the expected cost (Req 2.1).
	PerRequestFeeUSD Money `json:"per_request_fee_usd,omitempty"`
	// CacheTokensInclusive: whether the provider already includes the cached tokens
	// in InputTokens. It is DATA and not a constant so the assumption can be fixed
	// without a deploy — see Risks in the design.
	CacheTokensInclusive bool `json:"cache_tokens_inclusive,omitempty"`
}

// Candidate is a model eligible to serve the request.
type Candidate struct {
	Model    string
	Provider string // key for the credit and for the availability signal
	Caps     Capabilities
	Prices   PriceHistory

	// ModelID is the customer's DECLARATION of which weights this route serves
	// (e.g. "openai/gpt-5.2"). Routes sharing it are the same model, so swapping
	// between them carries no quality risk. Empty means "not declared", which
	// resolves as "not the same as anything" — identity is never inferred.
	ModelID string
	// Aggregator marks a provider that routes internally to varying upstreams and
	// may serve a different version or quantization between requests. Such a route
	// never joins an identity group, because the no-quality-risk promise of a
	// same-model swap would be false there.
	Aggregator bool
}

// RequestShape is the SHAPE of the request — never the content. Keeping the prompt
// out of here is what prevents content from leaking into the decision log.
type RequestShape struct {
	InputTokens       int
	CachedInputTokens int // 0 in the ex-ante decision; filled in the ex-post calculation
	MaxOutputTokens   int // ceiling requested by the client (0 = not informed)
	HasTools          bool
	HasImage          bool
	Feature           string
	RequestedModel    string // "" when the client did not inform it
}

// Policy is the slice of the effective config the decision needs.
type Policy struct {
	AllowedModels  []string
	ModelOrder     []string
	FeatureTiers   []string // tiers acceptable for this feature (empty = no restriction)
	FeatureModels  []string // optional, more specific than tiers
	EconomyMode    bool
	AutoCheapest   bool
	DefaultOutTok  int // heuristic for E[out] (Req 2.3)
	MinHintSamples int // below this the hint is ignored

	// Swap is the declared ceiling on how far a substitution may go. Empty keeps
	// the pre-feature behavior (any class permitted).
	//
	// It is enforced inside the ELIGIBILITY layer, not in the fallback loop. That
	// placement is the whole point: eligibility runs always — with or without
	// auto-cheapest, with or without an exhausted budget — so the restriction
	// covers all three substitution paths for free, instead of needing a duplicate
	// check in each one (which is exactly where a new path would forget it).
	Swap string

	// Bundle is the declared attempt order for this feature, in layers. nil keeps
	// ModelOrder as the only source of preference. It never widens what may serve —
	// see ApplyBundle.
	Bundle *Bundle
}

// Hints is the contract artifact published by Observability (Req 9).
// MedianOut is keyed by "feature|model" and "*|model".
type Hints struct {
	Version     int
	GeneratedAt time.Time
	// Samples is the ARTIFACT's count. It serves telemetry, not the decision of
	// whether a hint is trustworthy — see SamplesByKey.
	Samples   int
	MedianOut map[string]int
	// SamplesByKey is the per-key count for MedianOut. The confidence threshold has
	// to be evaluated ON THE KEY that will be used: a feature entry with 3 samples
	// may sit next to the org aggregate with hundreds, and discarding everything
	// because of the artifact's counter would throw away well-supported data.
	SamplesByKey map[string]int
	Unavailable  map[string]time.Time // model or provider → avoid until
}

// medianFor returns the median for a key, requiring the minimum sample count ON THE
// KEY. When SamplesByKey does not carry the key (artifact from an earlier version),
// it falls back to the artifact's counter — compatibility without requiring a
// republish.
func (h *Hints) medianFor(key string, minSamples int) (int, bool) {
	if h == nil {
		return 0, false
	}
	v, ok := h.MedianOut[key]
	if !ok || v <= 0 {
		return 0, false
	}
	n, hasN := h.SamplesByKey[key]
	if !hasN {
		n = h.Samples
	}
	if n < minSamples {
		return 0, false
	}
	return v, true
}

// Credit is a provider's declared balance (Req 15).
type Credit struct {
	RemainingUSD Money
	ExpiresAt    time.Time
}

type CreditState struct {
	ByProvider map[string]Credit
}

// Discard records why a candidate left the contest (Req 8.1).
type Discard struct {
	Model  string
	Reason string
}

// Decision is the result of the decision, with an auditable justification.
//
// ExpectedCostUSD and CashCostUSD are TWO fields on purpose: the monotonicity
// required by Req 2.6 is a property of the gross value, while credit zeroes out the
// cash (Req 16.1). Without separating them, the two requirements would contradict
// each other.
type Decision struct {
	Model                string
	ExpectedCostUSD      Money
	CashCostUSD          Money
	RequestedCostUSD     Money
	OutTokensSource      string
	PricingStatus        string
	Discards             []Discard
	AvailabilityDegraded bool
	EconomyMode          bool
	PaidFrom             string

	// SwapClass is the semantics of the substitution, when one happened. Empty
	// when the requested route was served.
	SwapClass string
	// ServedModelID is the declared identity of the served route, when declared.
	ServedModelID string
}

// allowedIn tells whether model is in the list; an empty list allows everything,
// preserving the semantics the router already uses in Config.allowed().
func allowedIn(list []string, model string) bool {
	if len(list) == 0 {
		return true
	}
	for _, m := range list {
		if m == model {
			return true
		}
	}
	return false
}
