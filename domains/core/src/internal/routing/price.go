// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// cacheReadFactorDefault: when the cache_read layer is not declared, it is derived
// from the standard input price (Req 5.6). 0.10 reflects the ~90% discount providers
// apply to prompt cache reads.
const cacheReadFactorDefault = 0.10

// Layer is a pair of prices per 1,000 tokens.
//
// The unit is USD per 1K tokens and it STAYS that way: it is what estimateCost()
// already uses in the router. Providers advertise per million; the conversion is a
// screen label, not a data change — migrating the unit would require rewriting every
// cost_usd already stored.
type Layer struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output,omitempty"`
}

// Date is a civil date (YYYY-MM-DD) in JSON. Price validity is a day, not an
// instant — using a raw time.Time would accept ambiguous formats.
type Date struct{ time.Time }

func (d *Date) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(bytes.TrimSpace(b)), `"`)
	if s == "" || s == "null" {
		d.Time = time.Time{}
		return nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		// Tolerate RFC3339 so data written by another tool is not rejected.
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return err
		}
	}
	d.Time = t.UTC()
	return nil
}

func (d Date) MarshalJSON() ([]byte, error) {
	if d.Time.IsZero() {
		return []byte(`""`), nil
	}
	return []byte(`"` + d.Time.UTC().Format("2006-01-02") + `"`), nil
}

// PriceTier is one price validity window with its layers.
//
// The `batch` layer exists already even though batch is out of scope: it is the seam
// that avoids migrating every org's config twice when batch arrives (design, Phasing
// section).
type PriceTier struct {
	EffectiveFrom Date  `json:"effective_from"`
	Standard      Layer `json:"standard"`
	CacheRead     Layer `json:"cache_read"`
	CacheWrite    Layer `json:"cache_write"`
	Batch         Layer `json:"batch"`

	// Source is the price's PROVENANCE: the provider's public table (`list`) or the
	// customer's negotiated price (`contract`). Absent = `list`.
	//
	// Why this matters for the ledger and is not just a label: a customer with a
	// commitment discount (EDP on AWS, committed-use on Google) pays LESS than the
	// table. Computing from the table makes the cost come out high and the savings
	// inflated in the same proportion — a systematic error in exactly the number
	// that backs gain-share. Recording the provenance is what lets us say, in an
	// audit, whether the cost came from the price the customer actually pays.
	Source string `json:"source,omitempty"`

	// CacheReadDerived marks that CacheRead was not declared but computed.
	// Not serialized: it is a result of resolution, not input data.
	CacheReadDerived bool `json:"-"`
}

// Price provenance.
const (
	PriceSourceList     = "list"     // provider's public table (default)
	PriceSourceContract = "contract" // negotiated price, informed by the customer
)

// SourceOf normalizes the provenance: empty or unknown counts as `list`.
//
// The default is `list` on purpose: it is the CONSERVATIVE hypothesis for our claims.
// Assuming a contract without the customer having declared one would make the
// platform assert a precision it does not have.
func (t PriceTier) SourceOf() string {
	if t.Source == PriceSourceContract {
		return PriceSourceContract
	}
	return PriceSourceList
}

// PriceHistory is every validity window of a model, ordered by EffectiveFrom.
type PriceHistory []PriceTier

// UnmarshalJSON accepts THREE shapes without requiring a migration of stored data
// (Req 5.5):
//
//  1. list of validity windows:  [ { "effective_from": "...", "standard": {...} }, ... ]
//  2. layers, single window: { "standard": {...}, "cache_read": {...} }
//  3. legacy shape: { "input": 0.003, "output": 0.015 }
//
// Shape 3 is what is stored today in every org's config.
func (h *PriceHistory) UnmarshalJSON(b []byte) error {
	t := bytes.TrimSpace(b)
	if len(t) == 0 || string(t) == "null" {
		*h = nil
		return nil
	}

	if t[0] == '[' { // (1)
		var tiers []PriceTier
		if err := json.Unmarshal(t, &tiers); err != nil {
			return err
		}
		sort.SliceStable(tiers, func(i, j int) bool {
			return tiers[i].EffectiveFrom.Before(tiers[j].EffectiveFrom.Time)
		})
		*h = tiers
		return nil
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(t, &probe); err != nil {
		return err
	}
	if _, layered := probe["standard"]; layered { // (2)
		var one PriceTier
		if err := json.Unmarshal(t, &one); err != nil {
			return err
		}
		*h = PriceHistory{one}
		return nil
	}

	// (3) legacy shape {input,output} — zero EffectiveFrom = always in effect.
	// `source` is read HERE too: the console stores prices in this shape, and an
	// Unmarshal into Layer alone would silently drop the provenance, making every
	// contract price show up as a list price.
	var legacy struct {
		Layer
		Source string `json:"source,omitempty"`
	}
	if err := json.Unmarshal(t, &legacy); err != nil {
		return err
	}
	*h = PriceHistory{{Standard: legacy.Layer, Source: legacy.Source}}
	return nil
}

// SelectPrice returns the window applicable at `now` and the price status.
//
// Status `unknown` does NOT remove the model from the catalog — it removes it from
// cost optimization (Req 3.2). The model is still servable if the customer asks for
// it explicitly.
func SelectPrice(h PriceHistory, now time.Time) (PriceTier, string) {
	var chosen PriceTier
	found := false
	for _, t := range h {
		// A future window never applies (Req 5.4). Zero = always in effect.
		if t.EffectiveFrom.IsZero() || !t.EffectiveFrom.After(now) {
			if !found || !t.EffectiveFrom.Before(chosen.EffectiveFrom.Time) {
				chosen, found = t, true
			}
		}
	}
	if !found {
		return PriceTier{}, PricingUnknown
	}
	// A zero or absent price is UNKNOWN, not "free" (Req 3.1). That is what keeps
	// the wizard's $0 default from beating auto-cheapest.
	if chosen.Standard.Input == 0 && chosen.Standard.Output == 0 {
		return chosen, PricingUnknown
	}

	status := PricingKnown
	if chosen.CacheRead.Input == 0 { // Req 5.6
		chosen.CacheRead.Input = chosen.Standard.Input * cacheReadFactorDefault
		chosen.CacheReadDerived = true
		status = PricingDerived
	}
	return chosen, status
}

// ExpectedCost computes the Expected_cost from the four components (Req 2.1).
//
// Used on BOTH sides: ex-ante in the decision (with CachedInputTokens = 0, because
// the cache counters only exist after the response) and ex-post in the realized
// cost. The same arithmetic on both sides is what keeps estimated_cost_usd and
// saved_usd from diverging.
//
// All coefficients are ≥ 0, which guarantees the monotonicity of Req 2.6.
func ExpectedCost(tier PriceTier, fee Money, inputTokens, cachedInputTokens, expectedOut int) Money {
	uncached := inputTokens - cachedInputTokens
	if uncached < 0 {
		uncached = 0
	}
	if cachedInputTokens < 0 {
		cachedInputTokens = 0
	}
	if expectedOut < 0 {
		expectedOut = 0
	}
	return float64(uncached)/1000*tier.Standard.Input +
		float64(cachedInputTokens)/1000*tier.CacheRead.Input +
		float64(expectedOut)/1000*tier.Standard.Output +
		fee
}

// RealizedCost is the ex-post cost, with the token partition already resolved and the
// cache write charged by its own layer (Req 4.3).
func RealizedCost(tier PriceTier, fee Money, split InputSplit, outputTokens int) Money {
	return float64(split.Uncached)/1000*tier.Standard.Input +
		float64(split.CacheRead)/1000*tier.CacheRead.Input +
		float64(split.CacheWrite)/1000*tier.CacheWrite.Input +
		float64(outputTokens)/1000*tier.Standard.Output +
		fee
}

// CacheSavings is the savings attributable to the provider's cache read: the
// difference between the standard input price and the cache_read price on the cached
// tokens (Req 4.6). Never negative.
func CacheSavings(tier PriceTier, cacheReadTokens int) Money {
	if cacheReadTokens <= 0 {
		return 0
	}
	d := tier.Standard.Input - tier.CacheRead.Input
	if d <= 0 {
		return 0
	}
	return float64(cacheReadTokens) / 1000 * d
}
