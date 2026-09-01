// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import (
	"errors"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"
)

var now = day("2026-08-09")

func cand(model, provider, tier string, toolUse, multi bool, ctx int, in, out float64) Candidate {
	return Candidate{
		Model:    model,
		Provider: provider,
		Caps: Capabilities{
			ToolUse: toolUse, Multimodal: multi, ContextWindow: ctx, Tier: tier,
		},
		Prices: PriceHistory{{Standard: Layer{Input: in, Output: out}}},
	}
}

// Catalog mirroring the real one: sonnet does tool use and is expensive; nova-micro
// is cheap and does NOT do tool use — exactly the pair that produced the bug in the
// test-production environment.
func catalogo() []Candidate {
	return []Candidate{
		cand("claude-sonnet", "bedrock", "frontier", true, true, 200_000, 0.003, 0.015),
		cand("claude-haiku", "bedrock", "balanced", true, false, 200_000, 0.001, 0.005),
		cand("nova-micro", "bedrock", "fast", false, false, 128_000, 0.000035, 0.00014),
	}
}

func autoPol() Policy {
	return Policy{AutoCheapest: true, DefaultOutTok: 512, ModelOrder: []string{
		"claude-sonnet", "claude-haiku", "nova-micro",
	}}
}

// --- the bug that gave rise to the feature -----------------------------------

func TestDecide_ComToolsNaoEscolheModeloSemToolUse(t *testing.T) {
	req := RequestShape{InputTokens: 400, HasTools: true, MaxOutputTokens: 256}
	d, err := Decide(catalogo(), autoPol(), nil, nil, req, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Model == "nova-micro" {
		t.Fatal("picked nova-micro for tool use — the bug is back")
	}
	if d.Model != "claude-haiku" {
		t.Errorf("chosen = %q, expected claude-haiku (the cheapest WITH tool use)", d.Model)
	}
	// The discard has to be auditable (Req 8.1).
	var achou bool
	for _, x := range d.Discards {
		if x.Model == "nova-micro" && x.Reason == DiscardNoToolUse {
			achou = true
		}
	}
	if !achou {
		t.Error("missing the record of nova-micro's discard with reason no_tool_use")
	}
}

func TestDecide_SemToolsEscolheOMaisBarato(t *testing.T) {
	req := RequestShape{InputTokens: 400, MaxOutputTokens: 256}
	d, err := Decide(catalogo(), autoPol(), nil, nil, req, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Model != "nova-micro" {
		t.Errorf("chosen = %q, expected nova-micro", d.Model)
	}
}

// Eligibility applies even with auto_cheapest OFF: a model's capability does not
// depend on cost optimization being active.
func TestDecide_ElegibilidadeValeSemAutoCheapest(t *testing.T) {
	pol := autoPol()
	pol.AutoCheapest = false
	req := RequestShape{InputTokens: 400, HasTools: true, RequestedModel: "nova-micro"}

	_, err := Decide(catalogo(), pol, nil, nil, req, now)
	if !errors.Is(err, ErrNoEligibleModel) {
		// nova-micro is the only requested one and it is ineligible; the others remain
		// eligible, so the decision does not fail — it ignores the impossible request.
		d, err2 := Decide(catalogo(), pol, nil, nil, req, now)
		if err2 != nil {
			t.Fatalf("unexpected error: %v", err2)
		}
		if d.Model == "nova-micro" {
			t.Fatal("served nova-micro with tools even without auto-cheapest")
		}
	}
}

func TestDecide_SemCandidatoElegivelDevolveErro(t *testing.T) {
	// Only nova-micro in the catalog and the request needs tool use.
	cands := []Candidate{cand("nova-micro", "bedrock", "fast", false, false, 128_000, 0.000035, 0.00014)}
	req := RequestShape{InputTokens: 100, HasTools: true}
	_, err := Decide(cands, autoPol(), nil, nil, req, now)
	if !errors.Is(err, ErrNoEligibleModel) {
		t.Errorf("error = %v, expected ErrNoEligibleModel", err)
	}
}

func TestDecide_PrecoZeroNaoVenceAOtimizacao(t *testing.T) {
	cands := append(catalogo(),
		Candidate{
			Model: "sem-preco", Provider: "bedrock",
			Caps:   Capabilities{ToolUse: true, ContextWindow: 100_000, Tier: "fast"},
			Prices: PriceHistory{{Standard: Layer{0, 0}}},
		})
	req := RequestShape{InputTokens: 400, MaxOutputTokens: 256}
	d, err := Decide(cands, autoPol(), nil, nil, req, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Model == "sem-preco" {
		t.Fatal("a model with no price won auto-cheapest — the bug is back")
	}
	if d.PricingStatus == PricingUnknown {
		t.Error("the chosen one should have a known or derived price")
	}
}

func TestDecide_JanelaDeContextoInsuficienteDescarta(t *testing.T) {
	req := RequestShape{InputTokens: 150_000, MaxOutputTokens: 1_000}
	d, err := Decide(catalogo(), autoPol(), nil, nil, req, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Model == "nova-micro" {
		t.Error("nova-micro (128k) would not fit 151k of context")
	}
}

func TestDecide_ContextWindowZeroNaoDescarta(t *testing.T) {
	cands := []Candidate{cand("desconhecido", "bedrock", "fast", true, true, 0, 0.001, 0.001)}
	req := RequestShape{InputTokens: 999_999, MaxOutputTokens: 4_096, HasTools: true}
	d, err := Decide(cands, autoPol(), nil, nil, req, now)
	if err != nil {
		t.Fatalf("an unknown context should not discard: %v", err)
	}
	if d.Model != "desconhecido" {
		t.Errorf("chosen = %q", d.Model)
	}
}

// --- availability (layer b) --------------------------------------------------

func TestDecide_DisponibilidadeDescartaEDegrada(t *testing.T) {
	amanha := now.AddDate(0, 0, 1)

	// One unavailable: it gets discarded.
	h := &Hints{Unavailable: map[string]time.Time{"nova-micro": amanha}}
	d, err := Decide(catalogo(), autoPol(), h, nil, RequestShape{InputTokens: 100}, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Model == "nova-micro" {
		t.Error("picked a model marked as unavailable")
	}
	if d.AvailabilityDegraded {
		t.Error("it should not degrade: there was still an available candidate")
	}

	// All unavailable: degrade instead of refusing (Req 10.5).
	hAll := &Hints{Unavailable: map[string]time.Time{"bedrock": amanha}}
	d2, err := Decide(catalogo(), autoPol(), hAll, nil, RequestShape{InputTokens: 100}, now)
	if err != nil {
		t.Fatalf("it should degrade, not fail: %v", err)
	}
	if !d2.AvailabilityDegraded {
		t.Error("AvailabilityDegraded should be true")
	}
	if d2.Model == "" {
		t.Error("it should have picked some model when degrading")
	}
}

// --- E[tokens_out] and its source --------------------------------------------

func TestDecide_OrigemDeExpectedOut(t *testing.T) {
	req := RequestShape{InputTokens: 100, Feature: "resumo"}

	d, _ := Decide(catalogo(), autoPol(), nil, nil, req, now)
	if d.OutTokensSource != OutSourceHeuristic {
		t.Errorf("with no hints: source = %q, expected heuristic", d.OutTokensSource)
	}

	h := &Hints{Samples: 100,
		MedianOut:    map[string]int{"resumo|nova-micro": 900},
		SamplesByKey: map[string]int{"resumo|nova-micro": 100}}
	d2, _ := Decide(catalogo(), autoPol(), h, nil, req, now)
	if d2.Model == "nova-micro" && d2.OutTokensSource != OutSourceHintOrgFeatureModel {
		t.Errorf("with a feature hint: source = %q", d2.OutTokensSource)
	}

	// A sample below the threshold is ignored: the median of 3 requests is worse than
	// an honest heuristic.
	pol := autoPol()
	pol.MinHintSamples = 50
	h2 := &Hints{Samples: 3,
		MedianOut:    map[string]int{"resumo|nova-micro": 900},
		SamplesByKey: map[string]int{"resumo|nova-micro": 3}}
	d3, _ := Decide(catalogo(), pol, h2, nil, req, now)
	if d3.OutTokensSource != OutSourceHeuristic {
		t.Errorf("small sample: source = %q, expected heuristic", d3.OutTokensSource)
	}
}

// The bug that validation in production exposed: the feature's item had 3 samples
// (below the threshold) but carried the org aggregate with 44. Evaluating the
// threshold on the ARTIFACT's counter discarded everything and the hint was never used.
func TestDecide_LimiarDeAmostraEhPorChaveNaoPorArtefato(t *testing.T) {
	pol := autoPol()
	pol.MinHintSamples = 20
	req := RequestShape{InputTokens: 100, Feature: "chat"}

	h := &Hints{
		Samples: 3, // artifact counter: small
		MedianOut: map[string]int{
			"chat|nova-micro": 36, // few samples on this feature
			"*|nova-micro":    61, // org aggregate, well supported
		},
		SamplesByKey: map[string]int{
			"chat|nova-micro": 3,
			"*|nova-micro":    44,
		},
	}
	d, err := Decide(catalogo(), pol, h, nil, req, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Model != "nova-micro" {
		t.Fatalf("chosen = %q, expected nova-micro", d.Model)
	}
	// The feature's key does not clear the threshold, but the org aggregate does: it has
	// to fall back to the aggregate, NOT to the heuristic.
	if d.OutTokensSource != OutSourceHintOrgModel {
		t.Errorf("source = %q, expected hint_org_model (the org aggregate)", d.OutTokensSource)
	}
}

// Compatibility: an artifact with no SamplesByKey (an earlier version) falls back to
// the artifact's counter, without requiring a republish.
func TestDecide_ArtefatoSemSamplesByKeyUsaContadorDoArtefato(t *testing.T) {
	pol := autoPol()
	pol.MinHintSamples = 20
	req := RequestShape{InputTokens: 100, Feature: "chat"}
	h := &Hints{Samples: 44, MedianOut: map[string]int{"*|nova-micro": 61}}
	d, _ := Decide(catalogo(), pol, h, nil, req, now)
	if d.OutTokensSource != OutSourceHintOrgModel {
		t.Errorf("source = %q, expected hint_org_model", d.OutTokensSource)
	}
}

// --- credit ------------------------------------------------------------------

func TestDecide_CreditoPreferidoMasNaoRessuscitaProibido(t *testing.T) {
	req := RequestShape{InputTokens: 1000, MaxOutputTokens: 200, HasTools: true}
	pol := autoPol()

	// Abundant credit on bedrock: the cash goes to zero and the most CAPABLE and
	// expensive one may be chosen, because in real money it costs zero.
	cs := &CreditState{ByProvider: map[string]Credit{
		"bedrock": {RemainingUSD: 1000, ExpiresAt: now.AddDate(0, 6, 0)},
	}}
	d, err := Decide(catalogo(), pol, nil, cs, req, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.PaidFrom != PaidFromCredit {
		t.Errorf("PaidFrom = %q, expected credit", d.PaidFrom)
	}
	if d.CashCostUSD != 0 {
		t.Errorf("CashCostUSD = %v, expected 0", d.CashCostUSD)
	}
	// Even with credit, never an ineligible model (Req 16.4).
	if d.Model == "nova-micro" {
		t.Fatal("credit resurrected a candidate discarded for lacking tool use")
	}

	// Expired credit does not count (Req 15.7).
	expirado := &CreditState{ByProvider: map[string]Credit{
		"bedrock": {RemainingUSD: 1000, ExpiresAt: now.AddDate(0, 0, -1)},
	}}
	d2, _ := Decide(catalogo(), pol, nil, expirado, req, now)
	if d2.PaidFrom != PaidFromCash {
		t.Errorf("expired credit: PaidFrom = %q, expected cash", d2.PaidFrom)
	}
}

// --- per-feature quality policy ----------------------------------------------

func TestDecide_PoliticaDeTierEModoEconomia(t *testing.T) {
	req := RequestShape{InputTokens: 100, MaxOutputTokens: 100}

	// The feature requires frontier: nova-micro and haiku are out.
	pol := autoPol()
	pol.FeatureTiers = []string{"frontier"}
	d, err := Decide(catalogo(), pol, nil, nil, req, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Model != "claude-sonnet" {
		t.Errorf("chosen = %q, expected claude-sonnet", d.Model)
	}

	// Economy mode allows a lower tier (Req 12.2).
	pol.EconomyMode = true
	d2, _ := Decide(catalogo(), pol, nil, nil, req, now)
	if d2.Model != "nova-micro" {
		t.Errorf("economy mode: chosen = %q, expected nova-micro", d2.Model)
	}
	if !d2.EconomyMode {
		t.Error("EconomyMode should be marked on the decision")
	}
}

// Bug found in the battery run against the test org: economy mode allowed ANY tier,
// turning the filter into a no-op. "I accept a worse response to spend less" must not
// turn into "you may spend more".
func TestDecide_ModoEconomiaSoLiberaTierInferior(t *testing.T) {
	pol := autoPol()
	pol.FeatureTiers = []string{"fast"} // accepts fast
	pol.EconomyMode = true
	req := RequestShape{InputTokens: 100, MaxOutputTokens: 100}

	d, err := Decide(catalogo(), pol, nil, nil, req, now)
	if err != nil {
		t.Fatal(err)
	}
	// Only nova-micro is fast; frontier and balanced have to stay OUT, even with
	// economy mode on.
	if d.Model != "nova-micro" {
		t.Errorf("chosen = %q, expected nova-micro (the only one in the fast tier)", d.Model)
	}
	for _, x := range d.Discards {
		if x.Model == "claude-sonnet" && x.Reason != DiscardTierNotAllowed {
			t.Errorf("claude-sonnet (frontier) should be discarded by tier, got %q", x.Reason)
		}
	}
}

// Bug found in the battery run: with auto_cheapest on, asking for a non-existent model
// started serving ANOTHER one silently. A typo in production would change the model
// that runs without the customer knowing.
func TestDecide_ModeloPedidoInexistenteDaErroEmVezDeSubstituir(t *testing.T) {
	req := RequestShape{InputTokens: 100, RequestedModel: "nao-existe"}
	_, err := Decide(catalogo(), autoPol(), nil, nil, req, now)
	if !errors.Is(err, ErrUnknownModel) {
		t.Errorf("error = %v, expected ErrUnknownModel", err)
	}
}

// The context check uses the output default when the client does not inform
// max_tokens — assuming zero would let through a model that does not fit the response.
func TestDecide_ContextoUsaSaidaPrevistaQuandoNaoHaTeto(t *testing.T) {
	cands := []Candidate{
		cand("minusculo", "bedrock", "fast", true, false, 300, 0.0000001, 0.0000001),
		cand("normal", "bedrock", "fast", true, false, 200_000, 0.001, 0.001),
	}
	pol := autoPol()
	pol.DefaultOutTok = 512
	// 100 of input, no max_tokens: 100 + 512 = 612 > 300 → minusculo is out, despite
	// being by far the cheapest.
	d, err := Decide(cands, pol, nil, nil, RequestShape{InputTokens: 100}, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Model != "normal" {
		t.Errorf("chosen = %q, expected normal (minusculo does not fit the response)", d.Model)
	}
}

// --- Property 1: eligibility is never violated (Req 1.2, 1.3, 1.4) -----------

type genCase struct {
	cands []Candidate
	pol   Policy
	req   RequestShape
}

func (genCase) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 1 + r.Intn(6)
	g := genCase{pol: Policy{AutoCheapest: true, DefaultOutTok: 512}}
	tiers := []string{"frontier", "balanced", "fast"}
	provs := []string{"bedrock", "anthropic", "google"}

	for i := 0; i < n; i++ {
		// Price decreasing with the index: the last ones are the cheapest. Combined
		// with random capabilities, this frequently produces the scenario where the
		// CHEAPEST is precisely the incapable one — which is what attacks P1.
		price := 0.05 / float64(i+1)
		g.cands = append(g.cands, Candidate{
			Model:    "m" + string(rune('a'+i)),
			Provider: provs[r.Intn(len(provs))],
			Caps: Capabilities{
				ToolUse:       r.Intn(2) == 0,
				Multimodal:    r.Intn(2) == 0,
				ContextWindow: []int{0, 8_000, 128_000, 200_000}[r.Intn(4)],
				Tier:          tiers[r.Intn(len(tiers))],
			},
			Prices: PriceHistory{{Standard: Layer{Input: price, Output: price * 3}}},
		})
		g.pol.ModelOrder = append(g.pol.ModelOrder, g.cands[i].Model)
	}
	g.req = RequestShape{
		InputTokens:     r.Intn(60_000),
		MaxOutputTokens: r.Intn(4_096),
		HasTools:        r.Intn(4) != 0, // high probability: attacks P1
		HasImage:        r.Intn(4) == 0,
	}
	// Half of the time it restricts allowed_models, to attack P2.
	if r.Intn(2) == 0 && n > 1 {
		g.pol.AllowedModels = []string{g.cands[r.Intn(n)].Model}
	}
	return reflect.ValueOf(g)
}

func TestProperty1_ElegibilidadeNuncaViolada(t *testing.T) {
	f := func(g genCase) bool {
		d, err := Decide(g.cands, g.pol, nil, nil, g.req, now)
		if errors.Is(err, ErrNoEligibleModel) {
			return true // refusing is a valid outcome
		}
		if err != nil {
			return false
		}
		c, ok := findCandidate(g.cands, d.Model)
		if !ok {
			return false // picked a model outside the catalog (Req 1.7)
		}
		if g.req.HasTools && !c.Caps.ToolUse {
			return false
		}
		if g.req.HasImage && !c.Caps.Multimodal {
			return false
		}
		if c.Caps.ContextWindow > 0 &&
			g.req.InputTokens+g.req.MaxOutputTokens > c.Caps.ContextWindow {
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// --- Property 2: the choice never leaves what is allowed (Req 1.6, 1.7, 11.3) -

func TestProperty2_NuncaForaDoPermitido(t *testing.T) {
	f := func(g genCase) bool {
		// Abundant credit on ALL providers: if the cost layer could resurrect a
		// forbidden candidate, this is the scenario that exposes it (Req 16.4).
		cs := &CreditState{ByProvider: map[string]Credit{
			"bedrock":   {RemainingUSD: 1e6, ExpiresAt: now.AddDate(1, 0, 0)},
			"anthropic": {RemainingUSD: 1e6, ExpiresAt: now.AddDate(1, 0, 0)},
			"google":    {RemainingUSD: 1e6, ExpiresAt: now.AddDate(1, 0, 0)},
		}}
		d, err := Decide(g.cands, g.pol, nil, cs, g.req, now)
		if errors.Is(err, ErrNoEligibleModel) {
			return true
		}
		if err != nil {
			return false
		}
		if !allowedIn(g.pol.AllowedModels, d.Model) {
			return false
		}
		_, ok := findCandidate(g.cands, d.Model)
		return ok
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// --- Property 7: credit with a floor of zero (Req 15.4, 16.1, 16.3) ----------

func TestProperty7_CreditoPisoZeroETeto2(t *testing.T) {
	f := func(declared, corrected, consumed, gross float64, expiredDays int) bool {
		// Normalize to plausible values (the default generator produces negatives).
		if declared < 0 {
			declared = -declared
		}
		if corrected < 0 {
			corrected = -corrected
		}
		if consumed < 0 {
			consumed = -consumed
		}
		if gross < 0 {
			gross = -gross
		}

		rem := Remaining(declared, corrected, consumed)
		if rem < 0 {
			return false // floor of zero (Req 15.4)
		}

		expires := now.AddDate(0, 0, expiredDays%365)
		cs := &CreditState{ByProvider: map[string]Credit{
			"bedrock": {RemainingUSD: rem, ExpiresAt: expires},
		}}
		cash, from := CashCost(gross, "bedrock", cs, now)

		expirado := now.After(expires)
		switch {
		case expirado || rem <= 0 || gross > rem:
			// Does not fit, exhausted or expired → real money (Req 15.6, 15.7, 16.3)
			return from == PaidFromCash && cash == gross
		default:
			// Covered → zero cash (Req 16.1)
			return from == PaidFromCredit && cash == 0
		}
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

func TestCreditExhausted(t *testing.T) {
	if !CreditExhausted(Credit{RemainingUSD: 0, ExpiresAt: now.AddDate(1, 0, 0)}, now) {
		t.Error("a zero balance should count as exhausted")
	}
	if !CreditExhausted(Credit{RemainingUSD: 100, ExpiresAt: now.AddDate(0, 0, -1)}, now) {
		t.Error("an expired credit should count as exhausted even with a balance")
	}
	if CreditExhausted(Credit{RemainingUSD: 100, ExpiresAt: now.AddDate(0, 1, 0)}, now) {
		t.Error("a valid credit with a balance should not count as exhausted")
	}
}
