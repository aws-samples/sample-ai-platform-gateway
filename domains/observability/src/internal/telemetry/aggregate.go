// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package telemetry is the PURE domain of Observability: the math of cost
// correlation, aggregation, SLI/SLO, burn rate and anomaly. No SDK, no network, no
// clock and no randomness — the reference instant always comes in as a parameter.
// That is what makes this math testable by property, offline, and independent of
// Lambda (hexagonal-refactor, R7, D2).
//
// This file gathers the types (Record, Agg) and the aggregations (Buckets, Series,
// Totals, availability SLI, average latency). It is a faithful move of what used to
// live in cmd/usage-api/main.go — no rule was rewritten.
package telemetry

import "sort"

// Record is the PURE projection of a Usage_Record persisted in the Cost_Store.
// Exported fields and NO AWS serialization tags: the adapter (adapters/ddbcoststore)
// is what reads the DynamoDB item and fills this in. That way a table schema change
// does not force a change in the domain (design, "Data Models").
type Record struct {
	Provider string
	Upstream string
	Model    string
	// RequestedModel is the model asked for (the baseline). When != Model a swap
	// happened (auto_cheapest/fallback/budget_degrade). RequestedCostUSD is what that
	// baseline would have cost on the same tokens — what makes counterfactual savings
	// auditable.
	RequestedModel   string
	RequestedCostUSD float64
	App              string
	Team             string
	Feature          string
	TokensIn         int
	TokensOut        int
	Cost             float64
	Saved            float64
	// CreditUSD/CashUSD partition the SAME Cost by pocket (provider credit vs cash
	// out). Adding them to Cost would count the quantity twice; they exist to separate
	// ROUTING savings from credit that was merely burned. CreditUSD+CashUSD==Cost.
	CreditUSD float64
	CashUSD   float64
	// SavedVerified/SavedCounterfactual partition Saved by STRENGTH OF PROOF.
	// Verified = cache (same model, observable avoided cost). Counterfactual = model
	// served != model requested. SavedVerified+SavedCounterfactual==Saved.
	SavedVerified       float64
	SavedCounterfactual float64
	// PriceSource: list | contract. CostList+CostContract==Cost in the aggregation.
	PriceSource string
	Reason      string // savings_reason (cache|auto_cheapest|fallback)
	Latency     int
	CacheHit    bool
	Status      string // success | error | blocked (empty = success, legacy record)
	FailReason  string // failure reason (rate_limit_exceeded, provider_down, ...)
	Detail      string // short text ONLY for provider errors (e.g. "provider 401: ..."); never contains a prompt
	Category    string // FCAPS: config|auth|policy|dependency|platform|capacity|ok
	SLIEligible bool   // counts toward the platform reliability SLI
	// Mode: sync | batch. Latency and SLI consider ONLY sync — batch has latency of
	// hours and would destroy both the P95 and the error budget. Empty is treated as sync.
	Mode string
	// SwapClass: "" (served as requested) | same_model | equivalent | downgrade.
	// It is the quality dimension of a substitution, which RequestedModel != Model
	// alone cannot express: that comparison says a swap happened, not whether the
	// model changed. ServedModelID is the declared identity of the served route.
	SwapClass     string
	ServedModelID string
	// Canary/CanaryRoute mark traffic sampled by a declared experiment. They exist
	// so the comparison can keep the two sides apart: without the mark, canary
	// requests would sit inside the baseline they are meant to be compared against.
	Canary      bool
	CanaryRoute string
	TS          string
}

// Swap classes, mirrored from the Core's decision vocabulary.
//
// Duplicated rather than imported on purpose: no domain imports another (verified
// by boundary_test.go). The shared meaning is pinned by the contract fixture, not
// by a runtime library.
const (
	SwapNone       = ""
	SwapSameModel  = "same_model"
	SwapEquivalent = "equivalent"
	SwapDowngrade  = "downgrade"
)

// IsSync tells whether the record belongs to the synchronous path (empty = sync, compat).
func (r Record) IsSync() bool { return r.Mode == "" || r.Mode == "sync" }

// Agg accumulates one slice of the correlation (by key: provider, model, app,
// feature or bucket). The JSON tags are identical to the original shell's — the HTTP
// response of /usage/summary does not change (R12).
type Agg struct {
	Key          string  `json:"key"`
	CostUSD      float64 `json:"cost_usd"`
	SavedUSD     float64 `json:"saved_usd"`
	CreditUSD    float64 `json:"credit_usd"`
	CashUSD      float64 `json:"cash_usd"`
	SavedVer     float64 `json:"saved_verified_usd"`
	SavedCf      float64 `json:"saved_counterfactual_usd"`
	CostList     float64 `json:"cost_list_price_usd"`
	CostContract float64 `json:"cost_contract_price_usd"`
	Requests     int     `json:"requests"`
	TokensIn     int     `json:"tokens_in"`
	TokensOut    int     `json:"tokens_out"`
	CacheHits    int     `json:"cache_hits"`
	latSum       int
}

// Add accumulates a record into the bucket, replicating exactly the legacy
// classification (a legacy record with no savings partition / no price_source / no
// paid_from is handled the conservative way that keeps the ledger reconcilable).
func (a *Agg) Add(r Record) {
	a.CostUSD += r.Cost
	a.SavedUSD += r.Saved
	// A legacy record (from before the partition existed) only has Saved. Classifying
	// it as COUNTERFACTUAL is the safe default, except for cache/provider_prompt_cache
	// (which are verifiable).
	if r.SavedVerified == 0 && r.SavedCounterfactual == 0 && r.Saved > 0 {
		// provider_arbitrage joins the verified list by the same test as the caches:
		// the model served IS the model requested (same declared identity through a
		// cheaper provider), so there is no assumed baseline.
		if r.Reason == "cache" || r.Reason == "provider_prompt_cache" || r.Reason == "provider_arbitrage" {
			a.SavedVer += r.Saved
		} else {
			a.SavedCf += r.Saved
		}
	} else {
		a.SavedVer += r.SavedVerified
		a.SavedCf += r.SavedCounterfactual
	}
	// A record with no price_source counts as list price (conservative assumption).
	if r.PriceSource == "contract" {
		a.CostContract += r.Cost
	} else {
		a.CostList += r.Cost
	}
	a.CreditUSD += r.CreditUSD
	// Legacy record with no paid_from: the cost is real cash out → cash (keeps
	// credit+cash==cost).
	if r.CreditUSD == 0 && r.CashUSD == 0 {
		a.CashUSD += r.Cost
	} else {
		a.CashUSD += r.CashUSD
	}
	a.Requests++
	a.TokensIn += r.TokensIn
	a.TokensOut += r.TokensOut
	if r.CacheHit {
		a.CacheHits++
	}
	a.latSum += r.Latency
}

// Totals aggregates every record into a single "total" bucket.
func Totals(recs []Record) Agg {
	total := Agg{Key: "total"}
	for _, r := range recs {
		total.Add(r)
	}
	return total
}

// Buckets aggregates by one dimension (a key-extracting function) and returns the
// result sorted by cost desc. An empty key becomes "none".
func Buckets(recs []Record, keyOf func(Record) string) []Agg {
	m := map[string]*Agg{}
	for _, r := range recs {
		k := keyOf(r)
		if k == "" {
			k = "none"
		}
		if m[k] == nil {
			m[k] = &Agg{Key: k}
		}
		m[k].Add(r)
	}
	list := make([]Agg, 0, len(m))
	for _, a := range m {
		list = append(list, *a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CostUSD > list[j].CostUSD })
	return list
}

// Series aggregates by time bucket (day|hour), sorted chronologically.
func Series(recs []Record, bucket string) []Agg {
	cut := 10 // YYYY-MM-DD
	if bucket == "hour" {
		cut = 13 // YYYY-MM-DDTHH
	}
	list := Buckets(recs, func(r Record) string {
		if len(r.TS) >= cut {
			return r.TS[:cut]
		}
		return r.TS
	})
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	return list
}

// SavingsByReasonSeries returns the savings by REASON over time. For each bucket
// (day|hour, in chronological order) it sums saved_usd of every savings_reason, and
// returns the aligned time labels plus a reason->values map (one per label, in the
// same order). Only records with Saved>0 and a non-empty reason are counted —
// savings with no reason are not attributed savings. This is the input for the
// per-mechanism savings charts (cache, auto_cheapest, fallback, budget_degrade) in
// the console.
func SavingsByReasonSeries(recs []Record, bucket string) (labels []string, byReason map[string][]float64) {
	cut := 10 // YYYY-MM-DD
	if bucket == "hour" {
		cut = 13 // YYYY-MM-DDTHH
	}
	keyOf := func(r Record) string {
		if len(r.TS) >= cut {
			return r.TS[:cut]
		}
		return r.TS
	}
	seen := map[string]bool{}
	for _, r := range recs {
		if r.Saved > 0 && r.Reason != "" {
			seen[keyOf(r)] = true
		}
	}
	labels = make([]string, 0, len(seen))
	for k := range seen {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	idx := make(map[string]int, len(labels))
	for i, k := range labels {
		idx[k] = i
	}
	byReason = map[string][]float64{}
	for _, r := range recs {
		if r.Saved <= 0 || r.Reason == "" {
			continue
		}
		if byReason[r.Reason] == nil {
			byReason[r.Reason] = make([]float64, len(labels))
		}
		byReason[r.Reason][idx[keyOf(r)]] += r.Saved
	}
	return labels, byReason
}

// BySwapClass breaks the period down by the KIND of substitution served.
//
// It answers the question a customer running a tuned reasoning flow actually asks:
// "how often did you serve me something other than what I asked, and how far off
// was it?" Cost and saving per class come along, which is what separates the
// arbitrage money (same model, cheaper provider) from the money that came with a
// quality tradeoff.
//
// The class of a record with no swap is reported as `none` (Buckets maps the empty
// key), so served-as-requested is an explicit line rather than a missing one.
func BySwapClass(recs []Record) []Agg {
	return Buckets(recs, func(r Record) string { return r.SwapClass })
}

// AvgLatencySync returns the average latency (integer) considering only the
// synchronous path. 0 when there is no synchronous request.
func AvgLatencySync(recs []Record) int {
	latSync, reqSync := 0, 0
	for _, r := range recs {
		if r.IsSync() {
			latSync += r.Latency
			reqSync++
		}
	}
	if reqSync > 0 {
		return latSync / reqSync
	}
	return 0
}

// SLI computes the availability SLI over the set of records. good = served/cache;
// eligible = success OR a failure with SLIEligible=true. Only the synchronous path
// is counted. Returns the failures-by-reason map (never nil).
func SLI(recs []Record) (good, denom int, failByReason map[string]int, pct float64) {
	failByReason = map[string]int{}
	for _, r := range recs {
		if !r.IsSync() {
			continue
		}
		isSuccess := r.Status == "" || r.Status == "success"
		eligible := isSuccess || r.SLIEligible
		if eligible {
			denom++
			if isSuccess {
				good++
			}
		}
		if !isSuccess && r.FailReason != "" {
			failByReason[r.FailReason]++
		}
	}
	pct = 100.0
	if denom > 0 {
		pct = float64(good) / float64(denom) * 100
	}
	return good, denom, failByReason, pct
}

// Served filters the served records (success/cache): blocked/error have zero cost
// and stay out of the cost summary (they live only in the Logs tab).
func Served(recs []Record) []Record {
	out := make([]Record, 0, len(recs))
	for _, r := range recs {
		if r.Status == "" || r.Status == "success" {
			out = append(out, r)
		}
	}
	return out
}
