// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package telemetry

import (
	"math"
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"testing/quick"
)

const eps = 1e-9

func almost(a, b float64) bool { return math.Abs(a-b) <= eps }

// corpus is the SAME fixed set from the usage-api characterization, now expressed in
// telemetry.Record (exported fields). The expected numbers below are identical to the
// ones pinned in the shell BEFORE the move — it is the proof that telemetry replicates
// the behavior (task 16, before removing the shell test in task 17).
func corpus() []Record {
	return []Record{
		{Provider: "openai", Model: "gpt-4o", App: "web", Team: "default", Feature: "chat",
			TokensIn: 100, TokensOut: 50, Cost: 1.0, PriceSource: "list",
			Latency: 200, Status: "success", Mode: "sync", TS: "2026-01-02T10:00:00Z"},
		{Provider: "openai", Model: "gpt-4o-mini", App: "web", Team: "default", Feature: "chat",
			TokensIn: 40, TokensOut: 20, Cost: 0.2, Saved: 0.8, SavedCounterfactual: 0.8,
			Reason: "auto_cheapest", PriceSource: "list",
			Latency: 100, Status: "success", Mode: "sync", TS: "2026-01-01T09:00:00Z"},
		{Provider: "bedrock", Model: "claude", App: "api", Team: "sre", Feature: "code",
			TokensIn: 10, TokensOut: 5, Cost: 0.5, Saved: 0.5, SavedVerified: 0.5,
			Reason: "cache", CacheHit: true, PriceSource: "contract",
			Latency: 0, Status: "success", Mode: "sync", TS: "2026-01-03T08:00:00Z"},
		{Provider: "openai", Model: "gpt-4o", App: "web", Team: "default", Feature: "chat",
			Cost: 0, Status: "error", FailReason: "provider_down", SLIEligible: true,
			Latency: 5000, Mode: "sync", TS: "2026-01-02T10:05:00Z"},
		{Provider: "openai", Model: "gpt-4o", App: "web", Team: "default", Feature: "chat",
			Cost: 0, Status: "blocked", FailReason: "rate_limit_exceeded", SLIEligible: false,
			Mode: "sync", TS: "2026-01-02T10:06:00Z"},
		{Provider: "openai", Model: "gpt-4o", App: "batch", Team: "default", Feature: "etl",
			Cost: 2.0, PriceSource: "list",
			Latency: 999999, Status: "success", Mode: "batch", TS: "2026-01-01T12:00:00Z"},
		{Provider: "bedrock", Model: "claude", App: "api", Team: "sre", Feature: "code",
			Cost: 1.0, CreditUSD: 1.0, PriceSource: "list",
			Latency: 300, Status: "success", Mode: "sync", TS: "2026-01-02T11:00:00Z"},
		{Provider: "anthropic", Model: "haiku", App: "api", Team: "sre", Feature: "code",
			Cost: 0.3, Saved: 0.3, Reason: "cache",
			Latency: 150, Status: "success", Mode: "sync", TS: "2026-01-03T07:00:00Z"},
	}
}

func TestSLI(t *testing.T) {
	good, denom, fbr, pct := SLI(corpus())
	if good != 5 || denom != 6 {
		t.Errorf("SLI good/denom = %d/%d, want 5/6", good, denom)
	}
	if !almost(pct, float64(5)/float64(6)*100) {
		t.Errorf("SLI pct = %v", pct)
	}
	if fbr["provider_down"] != 1 || fbr["rate_limit_exceeded"] != 1 || len(fbr) != 2 {
		t.Errorf("unexpected fail_by_reason: %#v", fbr)
	}
}

func TestAvgLatencySync(t *testing.T) {
	if got := AvgLatencySync(Served(corpus())); got != 150 {
		t.Errorf("AvgLatencySync = %d, want 150", got)
	}
}

func TestTotalsAndClosure(t *testing.T) {
	total := Totals(Served(corpus()))
	if !almost(total.CostUSD, 5.0) || !almost(total.SavedUSD, 1.6) {
		t.Errorf("cost/saved = %v/%v, want 5.0/1.6", total.CostUSD, total.SavedUSD)
	}
	if !almost(total.SavedVer, 0.8) || !almost(total.SavedCf, 0.8) {
		t.Errorf("saved ver/cf = %v/%v, want 0.8/0.8", total.SavedVer, total.SavedCf)
	}
	if !almost(total.SavedVer+total.SavedCf, total.SavedUSD) {
		t.Errorf("savings do not close: %v+%v != %v", total.SavedVer, total.SavedCf, total.SavedUSD)
	}
	if !almost(total.CostList, 4.5) || !almost(total.CostContract, 0.5) {
		t.Errorf("list/contract = %v/%v, want 4.5/0.5", total.CostList, total.CostContract)
	}
	if !almost(total.CostList+total.CostContract, total.CostUSD) {
		t.Errorf("price provenance does not close")
	}
	if !almost(total.CreditUSD, 1.0) || !almost(total.CashUSD, 4.0) {
		t.Errorf("credit/cash = %v/%v, want 1.0/4.0", total.CreditUSD, total.CashUSD)
	}
	if !almost(total.CreditUSD+total.CashUSD, total.CostUSD) {
		t.Errorf("pocket partition does not close")
	}
	if total.Requests != 6 || total.TokensIn != 150 || total.CacheHits != 1 {
		t.Errorf("requests/tokens/cache = %d/%d/%d, want 6/150/1", total.Requests, total.TokensIn, total.CacheHits)
	}
}

func TestByProvider(t *testing.T) {
	got := Buckets(Served(corpus()), func(r Record) string { return r.Provider })
	want := []struct {
		key  string
		cost float64
	}{{"openai", 3.2}, {"bedrock", 1.5}, {"anthropic", 0.3}}
	if len(got) != len(want) {
		t.Fatalf("by_provider has %d keys, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Key != w.key || !almost(got[i].CostUSD, w.cost) {
			t.Errorf("by_provider[%d]=%q/%v, want %q/%v", i, got[i].Key, got[i].CostUSD, w.key, w.cost)
		}
	}
}

func TestSeriesChronological(t *testing.T) {
	got := Series(Served(corpus()), "day")
	want := []struct {
		key  string
		cost float64
	}{{"2026-01-01", 2.2}, {"2026-01-02", 2.0}, {"2026-01-03", 0.8}}
	if len(got) != len(want) {
		t.Fatalf("series has %d buckets, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Key != w.key || !almost(got[i].CostUSD, w.cost) {
			t.Errorf("series[%d]=%q/%v, want %q/%v", i, got[i].Key, got[i].CostUSD, w.key, w.cost)
		}
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].Key < got[j].Key }) {
		t.Errorf("series out of chronological order")
	}
}

// ---- Property 8: order invariance + closure of the savings partition ----

// genRecords is a generator of WELL-FORMED records for testing/quick: integer values
// (exact float sums, no rounding noise) and the invariants the writer guarantees
// (Saved==Ver+Cf, Cost==Credit+Cash). That way the closure is exact and comparing by
// equality is legitimate.
func genRecords(rnd *rand.Rand) []Record {
	n := rnd.Intn(30)
	provs := []string{"openai", "bedrock", "anthropic", ""}
	models := []string{"gpt-4o", "claude", "haiku"}
	days := []string{"2026-01-01T", "2026-01-02T", "2026-01-03T"}
	reasons := []string{"cache", "auto_cheapest", "fallback", ""}
	out := make([]Record, n)
	for i := range out {
		ver := float64(rnd.Intn(6))
		cf := float64(rnd.Intn(6))
		credit := float64(rnd.Intn(6))
		cash := float64(rnd.Intn(6))
		src := "list"
		if rnd.Intn(2) == 0 {
			src = "contract"
		}
		out[i] = Record{
			Provider: provs[rnd.Intn(len(provs))],
			Model:    models[rnd.Intn(len(models))],
			App:      []string{"web", "api", "batch"}[rnd.Intn(3)],
			Team:     []string{"default", "sre"}[rnd.Intn(2)],
			Feature:  []string{"chat", "code", "etl"}[rnd.Intn(3)],
			TokensIn: rnd.Intn(100), TokensOut: rnd.Intn(100),
			Cost:                credit + cash,
			Saved:               ver + cf,
			SavedVerified:       ver,
			SavedCounterfactual: cf,
			CreditUSD:           credit,
			CashUSD:             cash,
			PriceSource:         src,
			Reason:              reasons[rnd.Intn(len(reasons))],
			Latency:             rnd.Intn(1000),
			Status:              "success",
			Mode:                "sync",
			TS:                  days[rnd.Intn(len(days))] + "10:00:00Z",
		}
	}
	return out
}

func aggMap(list []Agg) map[string]Agg {
	m := make(map[string]Agg, len(list))
	for _, a := range list {
		m[a.Key] = a
	}
	return m
}

func TestProp_BucketsOrderInvariant(t *testing.T) {
	f := func(seed int64) bool {
		rnd := rand.New(rand.NewSource(seed))
		recs := genRecords(rnd)
		// canonical permutations: reversed + shuffled.
		rev := make([]Record, len(recs))
		for i := range recs {
			rev[len(recs)-1-i] = recs[i]
		}
		shuf := append([]Record(nil), recs...)
		rand.New(rand.NewSource(seed+1)).Shuffle(len(shuf), func(i, j int) { shuf[i], shuf[j] = shuf[j], shuf[i] })

		keyOf := func(r Record) string { return r.Provider }
		base := aggMap(Buckets(recs, keyOf))
		return reflect.DeepEqual(base, aggMap(Buckets(rev, keyOf))) &&
			reflect.DeepEqual(base, aggMap(Buckets(shuf, keyOf)))
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Error(err)
	}
}

func TestProp_SeriesOrderInvariant(t *testing.T) {
	f := func(seed int64) bool {
		rnd := rand.New(rand.NewSource(seed))
		recs := genRecords(rnd)
		rev := make([]Record, len(recs))
		for i := range recs {
			rev[len(recs)-1-i] = recs[i]
		}
		return reflect.DeepEqual(Series(recs, "day"), Series(rev, "day"))
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Error(err)
	}
}

func TestProp_PartitionCloses(t *testing.T) {
	f := func(seed int64) bool {
		rnd := rand.New(rand.NewSource(seed))
		total := Totals(genRecords(rnd))
		return almost(total.SavedVer+total.SavedCf, total.SavedUSD) &&
			almost(total.CreditUSD+total.CashUSD, total.CostUSD) &&
			almost(total.CostList+total.CostContract, total.CostUSD)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 500}); err != nil {
		t.Error(err)
	}
}
