// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import (
	"encoding/json"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

// --- tolerant deserialization (Req 5.5) --------------------------------------

func TestPriceHistory_AceitaAsTresFormas(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantTiers int
		wantIn    float64 // Standard.Input of the first effective tier
	}{
		{
			name:      "old input/output shape (what is stored today)",
			in:        `{"input":0.003,"output":0.015}`,
			wantTiers: 1,
			wantIn:    0.003,
		},
		{
			name:      "layers with a single effective date",
			in:        `{"standard":{"input":0.002,"output":0.01},"cache_read":{"input":0.0002}}`,
			wantTiers: 1,
			wantIn:    0.002,
		},
		{
			name: "list of effective dates",
			in: `[{"effective_from":"2026-09-01","standard":{"input":0.003,"output":0.015}},
			      {"effective_from":"2026-08-01","standard":{"input":0.002,"output":0.010}}]`,
			wantTiers: 2,
			wantIn:    0.002, // sorted by effective_from: August comes first
		},
		{name: "null becomes empty", in: `null`, wantTiers: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var h PriceHistory
			if err := json.Unmarshal([]byte(tc.in), &h); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if len(h) != tc.wantTiers {
				t.Fatalf("len = %d, expected %d", len(h), tc.wantTiers)
			}
			if tc.wantTiers > 0 && math.Abs(h[0].Standard.Input-tc.wantIn) > 1e-12 {
				t.Errorf("Standard.Input = %v, expected %v", h[0].Standard.Input, tc.wantIn)
			}
		})
	}
}

// --- SelectPrice -------------------------------------------------------------

func TestSelectPrice(t *testing.T) {
	// Real case: Sonnet 5 at $2/$10 per million until 2026-08-31 and $3/$15 from
	// 2026-09-01. In USD per 1K: 0.002/0.010 and 0.003/0.015.
	sonnet := PriceHistory{
		{EffectiveFrom: Date{day("2026-08-01")}, Standard: Layer{0.002, 0.010}},
		{EffectiveFrom: Date{day("2026-09-01")}, Standard: Layer{0.003, 0.015}},
	}

	tests := []struct {
		name       string
		h          PriceHistory
		now        time.Time
		wantIn     float64
		wantStatus string
	}{
		{"current effective tier", sonnet, day("2026-08-09"), 0.002, PricingDerived},
		{"a future tier takes over on its date", sonnet, day("2026-09-01"), 0.003, PricingDerived},
		{"after the switch", sonnet, day("2026-10-15"), 0.003, PricingDerived},
		{
			"before any effective date it is unknown",
			sonnet, day("2026-07-01"), 0, PricingUnknown,
		},
		{
			"the old shape always applies",
			PriceHistory{{Standard: Layer{0.001, 0.005}}}, day("2020-01-01"), 0.001, PricingDerived,
		},
		{
			"a price of zero is unknown, not free (wizard bug)",
			PriceHistory{{Standard: Layer{0, 0}}}, day("2026-08-09"), 0, PricingUnknown,
		},
		{
			"a declared cache_read is not derived",
			PriceHistory{{Standard: Layer{0.002, 0.01}, CacheRead: Layer{Input: 0.0002}}},
			day("2026-08-09"), 0.002, PricingKnown,
		},
		{"an empty history is unknown", nil, day("2026-08-09"), 0, PricingUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tier, st := SelectPrice(tc.h, tc.now)
			if st != tc.wantStatus {
				t.Errorf("status = %q, expected %q", st, tc.wantStatus)
			}
			if tc.wantStatus != PricingUnknown && math.Abs(tier.Standard.Input-tc.wantIn) > 1e-12 {
				t.Errorf("Standard.Input = %v, expected %v", tier.Standard.Input, tc.wantIn)
			}
		})
	}
}

func TestSelectPrice_DerivaCacheReadEm10PorCento(t *testing.T) {
	tier, st := SelectPrice(PriceHistory{{Standard: Layer{0.003, 0.015}}}, day("2026-08-09"))
	if st != PricingDerived {
		t.Fatalf("status = %q, expected %q", st, PricingDerived)
	}
	if math.Abs(tier.CacheRead.Input-0.0003) > 1e-12 {
		t.Errorf("CacheRead.Input = %v, expected 0.0003", tier.CacheRead.Input)
	}
	if !tier.CacheReadDerived {
		t.Error("CacheReadDerived should be true")
	}
}

// --- ExpectedCost ------------------------------------------------------------

func TestExpectedCost_UnidadePor1kTokens(t *testing.T) {
	tier := PriceTier{Standard: Layer{0.003, 0.015}, CacheRead: Layer{Input: 0.0003}}
	// 1000 in (none cached) + 1000 out = 0.003 + 0.015
	got := ExpectedCost(tier, 0, 1000, 0, 1000)
	if math.Abs(got-0.018) > 1e-12 {
		t.Errorf("ExpectedCost = %v, expected 0.018", got)
	}
}

func TestExpectedCost_CacheReduzOCusto(t *testing.T) {
	tier := PriceTier{Standard: Layer{0.003, 0.015}, CacheRead: Layer{Input: 0.0003}}
	semCache := ExpectedCost(tier, 0, 1000, 0, 100)
	comCache := ExpectedCost(tier, 0, 1000, 900, 100)
	if !(comCache < semCache) {
		t.Errorf("with cache (%v) should cost less than without cache (%v)", comCache, semCache)
	}
}

func TestExpectedCost_TaxaPorRequisicaoEntraNaConta(t *testing.T) {
	tier := PriceTier{Standard: Layer{0.001, 0.001}}
	sem := ExpectedCost(tier, 0, 100, 0, 100)
	com := ExpectedCost(tier, 0.005, 100, 0, 100)
	if math.Abs((com-sem)-0.005) > 1e-12 {
		t.Errorf("difference = %v, expected 0.005", com-sem)
	}
}

// --- Property 3: gross cost is monotonically non-decreasing (Req 2.6) --------

// The monotonicity is about the PORTIONS of the split (uncached, cache_read,
// cache_write, out), not about the pair (InputTokens, CachedInputTokens) — in that pair
// one grows at the expense of the other, and moving a token into cache REDUCES the
// cost, which is the whole point of the cache. A statement that ignores this is
// falsifiable, and it was.
type genCost struct {
	tier PriceTier
	fee  Money
	a, b InputSplit // a ≤ b component by component
	outA int
	outB int
}

func (genCost) Generate(r *rand.Rand, _ int) reflect.Value {
	g := genCost{
		tier: PriceTier{
			Standard:   Layer{Input: r.Float64() * 0.02, Output: r.Float64() * 0.1},
			CacheRead:  Layer{Input: r.Float64() * 0.002},
			CacheWrite: Layer{Input: r.Float64() * 0.03},
		},
		fee: r.Float64() * 0.01,
	}
	g.a = InputSplit{Uncached: r.Intn(50_000), CacheRead: r.Intn(20_000), CacheWrite: r.Intn(5_000)}
	// b ≥ a component by component
	g.b = InputSplit{
		Uncached:   g.a.Uncached + r.Intn(10_000),
		CacheRead:  g.a.CacheRead + r.Intn(10_000),
		CacheWrite: g.a.CacheWrite + r.Intn(2_000),
	}
	g.outA = r.Intn(4_096)
	g.outB = g.outA + r.Intn(2_000)
	return reflect.ValueOf(g)
}

func TestProperty3_CustoMonotonicoNasParcelas(t *testing.T) {
	f := func(g genCost) bool {
		const eps = 1e-9
		menor := RealizedCost(g.tier, g.fee, g.a, g.outA)
		maior := RealizedCost(g.tier, g.fee, g.b, g.outB)
		if maior < menor-eps {
			return false
		}
		// ExpectedCost: raising the input (cached ones fixed) makes the non-cached
		// portion grow; raising the output likewise. Neither of them reduces it.
		base := ExpectedCost(g.tier, g.fee, g.a.Uncached+g.a.CacheRead, g.a.CacheRead, g.outA)
		maisIn := ExpectedCost(g.tier, g.fee, g.b.Uncached+g.a.CacheRead, g.a.CacheRead, g.outA)
		maisOut := ExpectedCost(g.tier, g.fee, g.a.Uncached+g.a.CacheRead, g.a.CacheRead, g.outB)
		return maisIn >= base-eps && maisOut >= base-eps
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// Sibling property: under COHERENT pricing (cache_read ≤ standard.input — what every
// provider practices), moving a token from non-cached to cache_read REDUCES the cost.
// That is what gives the cache its economic sense.
type genCoherent struct {
	tier   PriceTier
	fee    Money
	in     int
	c1, c2 int // c1 ≤ c2 ≤ in
	out    int
}

func (genCoherent) Generate(r *rand.Rand, _ int) reflect.Value {
	std := 0.0001 + r.Float64()*0.02
	g := genCoherent{
		tier: PriceTier{
			Standard: Layer{Input: std, Output: r.Float64() * 0.1},
			// realistic cache-read discount factor: 5% to 50%
			CacheRead: Layer{Input: std * (0.05 + r.Float64()*0.45)},
		},
		fee: r.Float64() * 0.01,
		in:  1 + r.Intn(50_000),
		out: r.Intn(4_096),
	}
	g.c1 = r.Intn(g.in + 1)
	g.c2 = g.c1 + r.Intn(g.in-g.c1+1)
	return reflect.ValueOf(g)
}

func TestProperty3Irma_CacheCoerenteReduzCusto(t *testing.T) {
	f := func(g genCoherent) bool {
		const eps = 1e-9
		menosCache := ExpectedCost(g.tier, g.fee, g.in, g.c1, g.out)
		maisCache := ExpectedCost(g.tier, g.fee, g.in, g.c2, g.out)
		return maisCache <= menosCache+eps
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// An INCOHERENT config (cache more expensive than standard input) is NOT silently
// normalized: the cost goes up, and CacheSavings returns zero so the ledger does not
// report negative savings. Normalizing behind the scenes would charge less than the
// provider charges.
func TestPrecoIncoerente_NaoEhNormalizadoEmSilencio(t *testing.T) {
	tier := PriceTier{Standard: Layer{Input: 0.001, Output: 0.001}, CacheRead: Layer{Input: 0.005}}
	semCache := ExpectedCost(tier, 0, 1000, 0, 0)
	comCache := ExpectedCost(tier, 0, 1000, 1000, 0)
	if !(comCache > semCache) {
		t.Errorf("with an incoherent cache (%v) it should cost MORE than without (%v)", comCache, semCache)
	}
	if got := CacheSavings(tier, 1000); got != 0 {
		t.Errorf("CacheSavings = %v, expected 0 (never negative)", got)
	}
}

// Determinism and independence from credit (Property 3 of the design).
func TestExpectedCost_DeterministicoEIndependenteDeCredito(t *testing.T) {
	tier := PriceTier{Standard: Layer{0.003, 0.015}, CacheRead: Layer{Input: 0.0003}}
	a := ExpectedCost(tier, 0.001, 1000, 200, 300)
	b := ExpectedCost(tier, 0.001, 1000, 200, 300)
	if a != b {
		t.Errorf("not deterministic: %v vs %v", a, b)
	}
	// Credit does not change the GROSS cost: it only projects the cash (Req 15.5).
	cs := &CreditState{ByProvider: map[string]Credit{
		"bedrock": {RemainingUSD: 1000, ExpiresAt: day("2027-01-01")},
	}}
	cash, from := CashCost(a, "bedrock", cs, day("2026-08-09"))
	if cash != 0 || from != PaidFromCredit {
		t.Errorf("CashCost = (%v, %q), expected (0, credit)", cash, from)
	}
	if c := ExpectedCost(tier, 0.001, 1000, 200, 300); c != a {
		t.Errorf("gross cost changed because of credit: %v vs %v", c, a)
	}
}

// --- Property 4: the tier is always the most recent one already in effect
// (Req 5.3, 5.4) --------------------------------------------------------------

type genHistory struct {
	h   PriceHistory
	now time.Time
}

func (genHistory) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 1 + r.Intn(5)
	base := day("2026-01-01")
	g := genHistory{now: base.AddDate(0, r.Intn(24), r.Intn(28))}
	for i := 0; i < n; i++ {
		// Dates out of chronological order ON PURPOSE: that is what attacks an
		// implementation that takes the first or the minimum instead of the most recent.
		g.h = append(g.h, PriceTier{
			EffectiveFrom: Date{base.AddDate(0, r.Intn(30), r.Intn(28))},
			Standard:      Layer{Input: 0.0001 + r.Float64(), Output: 0.0001 + r.Float64()},
		})
	}
	return reflect.ValueOf(g)
}

func TestProperty4_VigenciaMaisRecenteJaIniciada(t *testing.T) {
	f := func(g genHistory) bool {
		tier, st := SelectPrice(g.h, g.now)

		// Is there any tier already in effect?
		var maxStarted time.Time
		any := false
		for _, x := range g.h {
			ef := x.EffectiveFrom.Time
			if ef.IsZero() || !ef.After(g.now) {
				if !any || ef.After(maxStarted) {
					maxStarted, any = ef, true
				}
			}
		}

		if !any {
			// None in effect → necessarily unknown (Req 5.4).
			return st == PricingUnknown
		}
		if st == PricingUnknown {
			// Only acceptable if the chosen price is zero (Req 3.1).
			return tier.Standard.Input == 0 && tier.Standard.Output == 0
		}
		// It can never have picked a future tier.
		if tier.EffectiveFrom.After(g.now) {
			return false
		}
		// And it has to be the most recent among those already in effect.
		return tier.EffectiveFrom.Time.Equal(maxStarted)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// --- CacheSavings ------------------------------------------------------------

func TestCacheSavings_NuncaNegativa(t *testing.T) {
	// A cache_read more expensive than the standard one (an incoherent config) must not
	// produce negative "savings" nor inflate the ledger.
	tier := PriceTier{Standard: Layer{Input: 0.001}, CacheRead: Layer{Input: 0.005}}
	if got := CacheSavings(tier, 1000); got != 0 {
		t.Errorf("CacheSavings = %v, expected 0", got)
	}
	tier2 := PriceTier{Standard: Layer{Input: 0.003}, CacheRead: Layer{Input: 0.0003}}
	if got := CacheSavings(tier2, 1000); math.Abs(got-0.0027) > 1e-12 {
		t.Errorf("CacheSavings = %v, expected 0.0027", got)
	}
	if got := CacheSavings(tier2, 0); got != 0 {
		t.Errorf("CacheSavings(0) = %v, expected 0", got)
	}
}
