// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import (
	"context"
	"errors"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/aiplat/core/internal/ports"
)

// fakeProvider is the reason ports.Provider exists: without it, escalation would only
// be testable against the cloud — precisely the path where the cost doubles.
type fakeProvider struct {
	byModel map[string]ports.Result
	errFor  map[string]error
	calls   []string
}

func (f *fakeProvider) Invoke(_ context.Context, in ports.InvokeInput) (ports.Result, error) {
	f.calls = append(f.calls, in.Model)
	if err, ok := f.errFor[in.Model]; ok {
		return ports.Result{}, err
	}
	return f.byModel[in.Model], nil
}

func fixedCost(m map[string]Money) CostFn {
	return func(model string, _ ports.Result) Money { return m[model] }
}

func nextFixed(c Candidate, ok bool) NextTierFn {
	return func(string) (Candidate, bool) { return c, ok }
}

var ctx = context.Background()

// --- Validate ---------------------------------------------------------------

func TestValidate(t *testing.T) {
	toolShape := RequestShape{HasTools: true}
	tests := []struct {
		name       string
		res        ports.Result
		shape      RequestShape
		expectJSON bool
		wantValid  bool
		wantReason string
	}{
		{"ordinary text is valid", ports.Result{Text: "ok"}, RequestShape{}, false, true, ""},
		{
			"asked for tools and got nothing back",
			ports.Result{}, toolShape, false, false, InvalidMissingToolCalls,
		},
		{
			"valid tool call",
			ports.Result{ToolCalls: []ports.ToolCall{{ID: "1", Name: "get_weather", Arguments: `{"location":"x"}`}}},
			toolShape, false, true, "",
		},
		{
			"a nameless tool call is broken",
			ports.Result{ToolCalls: []ports.ToolCall{{ID: "1", Name: "  "}}},
			toolShape, false, false, InvalidMissingToolCalls,
		},
		{
			"empty response without tools",
			ports.Result{Text: "   "}, RequestShape{}, false, false, InvalidEmptyResponse,
		},
		{
			"truncated by max_tokens",
			ports.Result{Text: "come...", StopReason: "max_tokens"}, RequestShape{}, false, false, InvalidTruncated,
		},
		{
			"invalid json when json was expected",
			ports.Result{Text: "não é json"}, RequestShape{}, true, false, InvalidJSON,
		},
		{
			"valid json when json was expected",
			ports.Result{Text: `{"a":1}`}, RequestShape{}, true, true, "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := Validate(tc.res, tc.shape, tc.expectJSON)
			if v.Valid != tc.wantValid || v.Reason != tc.wantReason {
				t.Errorf("Validate = {%v %q}, expected {%v %q}", v.Valid, v.Reason, tc.wantValid, tc.wantReason)
			}
		})
	}
}

// --- Escalate ---------------------------------------------------------------

func TestEscalate_RespostaValidaNaoEscala(t *testing.T) {
	p := &fakeProvider{byModel: map[string]ports.Result{"fast": {Text: "ok"}}}
	out, err := Escalate(ctx, p, ports.InvokeInput{Model: "fast"}, RequestShape{},
		fixedCost(map[string]Money{"fast": 0}), nextFixed(Candidate{Model: "strong"}, true), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Escalated || len(out.Attempts) != 1 {
		t.Errorf("should not escalate: escalated=%v attempts=%d", out.Escalated, len(out.Attempts))
	}
	if len(p.calls) != 1 {
		t.Errorf("called %d times, expected 1", len(p.calls))
	}
}

func TestEscalate_ToolCallVazioEscalaEAcerta(t *testing.T) {
	p := &fakeProvider{byModel: map[string]ports.Result{
		"fast":   {}, // empty: invalid
		"strong": {ToolCalls: []ports.ToolCall{{ID: "1", Name: "get_weather", Arguments: `{"location":"Seattle"}`}}},
	}}
	cost := fixedCost(map[string]Money{"fast": 0.001, "strong": 0.010})
	out, err := Escalate(ctx, p, ports.InvokeInput{Model: "fast"}, RequestShape{HasTools: true},
		cost, nextFixed(Candidate{Model: "strong"}, true), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Escalated {
		t.Fatal("should have escalated")
	}
	if len(out.Attempts) != 2 {
		t.Fatalf("attempts = %d, expected 2", len(out.Attempts))
	}
	if out.Reason != InvalidMissingToolCalls {
		t.Errorf("reason = %q", out.Reason)
	}
	// The cost of BOTH attempts: that is what the ledger has to discount.
	if out.TotalCost != 0.011 {
		t.Errorf("TotalCost = %v, expected 0.011", out.TotalCost)
	}
	if out.Final().Model != "strong" {
		t.Errorf("the final response should come from strong, came from %q", out.Final().Model)
	}
	if out.Outcome != EscalationNone {
		t.Errorf("Outcome = %q, expected empty (the escalation succeeded)", out.Outcome)
	}
}

func TestEscalate_DesabilitadoRegistraEmVezDeSilenciar(t *testing.T) {
	p := &fakeProvider{byModel: map[string]ports.Result{"fast": {}}}
	out, err := Escalate(ctx, p, ports.InvokeInput{Model: "fast"}, RequestShape{HasTools: true},
		fixedCost(map[string]Money{"fast": 0.001}), nextFixed(Candidate{Model: "strong"}, true), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Escalated {
		t.Error("should not escalate when escalation is disabled")
	}
	if out.Outcome != ValidationFailed {
		t.Errorf("Outcome = %q, expected validation_failed", out.Outcome)
	}
	if len(p.calls) != 1 {
		t.Errorf("called %d times, expected 1", len(p.calls))
	}
}

func TestEscalate_SemTierSuperior(t *testing.T) {
	p := &fakeProvider{byModel: map[string]ports.Result{"top": {}}}
	out, err := Escalate(ctx, p, ports.InvokeInput{Model: "top"}, RequestShape{HasTools: true},
		fixedCost(map[string]Money{"top": 0.02}), nextFixed(Candidate{}, false), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != EscalationUnavailable {
		t.Errorf("Outcome = %q, expected escalation_unavailable", out.Outcome)
	}
}

func TestEscalate_SegundaTentativaTambemInvalida(t *testing.T) {
	p := &fakeProvider{byModel: map[string]ports.Result{"fast": {}, "strong": {}}}
	out, err := Escalate(ctx, p, ports.InvokeInput{Model: "fast"}, RequestShape{HasTools: true},
		fixedCost(map[string]Money{"fast": 0.001, "strong": 0.010}),
		nextFixed(Candidate{Model: "strong"}, true), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != EscalationFailed {
		t.Errorf("Outcome = %q, expected escalation_failed", out.Outcome)
	}
	// Charge for both: the customer paid for both calls.
	if out.TotalCost != 0.011 {
		t.Errorf("TotalCost = %v, expected 0.011", out.TotalCost)
	}
}

func TestEscalate_NoMaximoUmaTentativaDeEscalonamento(t *testing.T) {
	p := &fakeProvider{byModel: map[string]ports.Result{"a": {}, "b": {}}}
	out, _ := Escalate(ctx, p, ports.InvokeInput{Model: "a"}, RequestShape{HasTools: true},
		fixedCost(map[string]Money{"a": 1, "b": 1}), nextFixed(Candidate{Model: "b"}, true), true, false)
	if len(out.Attempts) > 2 {
		t.Errorf("attempts = %d, the ceiling is 2", len(out.Attempts))
	}
	if len(p.calls) > 2 {
		t.Errorf("called %d times, the ceiling is 2", len(p.calls))
	}
}

func TestEscalate_ErroDeTransporteNaPrimeiraPropaga(t *testing.T) {
	boom := errors.New("provider down")
	p := &fakeProvider{errFor: map[string]error{"fast": boom}}
	_, err := Escalate(ctx, p, ports.InvokeInput{Model: "fast"}, RequestShape{},
		fixedCost(map[string]Money{"fast": 0}), nextFixed(Candidate{}, false), true, false)
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, expected it to propagate %v", err, boom)
	}
}

func TestEscalate_ErroNaSegundaNaoCobraTentativaSemResposta(t *testing.T) {
	p := &fakeProvider{
		byModel: map[string]ports.Result{"fast": {}},
		errFor:  map[string]error{"strong": errors.New("timeout")},
	}
	out, err := Escalate(ctx, p, ports.InvokeInput{Model: "fast"}, RequestShape{HasTools: true},
		fixedCost(map[string]Money{"fast": 0.001, "strong": 0.010}),
		nextFixed(Candidate{Model: "strong"}, true), true, false)
	if err != nil {
		t.Fatalf("should not propagate: the 1st response exists (%v)", err)
	}
	if out.TotalCost != 0.001 {
		t.Errorf("TotalCost = %v, expected 0.001 (an attempt with no response is not charged)", out.TotalCost)
	}
	if out.Outcome != EscalationFailed {
		t.Errorf("Outcome = %q", out.Outcome)
	}
}

// --- NextTierFrom -----------------------------------------------------------

func TestNextTierFrom_EscolheOMaisBaratoDoTierSuperior(t *testing.T) {
	cands := []Candidate{
		cand("fast-a", "bedrock", "fast", true, false, 0, 0.0001, 0.0001),
		cand("bal-caro", "bedrock", "balanced", true, false, 0, 0.010, 0.010),
		cand("bal-barato", "bedrock", "balanced", true, false, 0, 0.002, 0.002),
		cand("front", "bedrock", "frontier", true, false, 0, 0.050, 0.050),
	}
	costOf := func(c Candidate) (Money, bool) {
		tier, st := SelectPrice(c.Prices, now)
		if st == PricingUnknown {
			return 0, false
		}
		return ExpectedCost(tier, 0, 1000, 0, 500), true
	}
	got, ok := NextTierFrom(cands, "fast-a", costOf)("fast-a")
	if !ok {
		t.Fatal("should find a higher tier")
	}
	if got.Model != "bal-barato" {
		t.Errorf("chosen = %q, expected bal-barato (the cheapest of the tier above)", got.Model)
	}

	// From the top there is nowhere to escalate.
	if _, ok := NextTierFrom(cands, "front", costOf)("front"); ok {
		t.Error("from frontier there should be no higher tier")
	}
}

func TestNextTierFrom_IgnoraPrecoDesconhecido(t *testing.T) {
	cands := []Candidate{
		cand("fast-a", "bedrock", "fast", true, false, 0, 0.0001, 0.0001),
		{Model: "sem-preco", Provider: "bedrock",
			Caps:   Capabilities{Tier: "frontier", ToolUse: true},
			Prices: PriceHistory{{Standard: Layer{0, 0}}}},
	}
	costOf := func(c Candidate) (Money, bool) {
		tier, st := SelectPrice(c.Prices, now)
		if st == PricingUnknown {
			return 0, false
		}
		return ExpectedCost(tier, 0, 1000, 0, 500), true
	}
	if _, ok := NextTierFrom(cands, "fast-a", costOf)("fast-a"); ok {
		t.Error("a model with no price should not be an escalation target")
	}
}

// --- Property 6: net savings never negative (Req 14.8, 14.9) ----------------

type genEsc struct {
	requestedCost Money
	costA, costB  Money
	escalate      bool
}

func (genEsc) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(genEsc{
		requestedCost: r.Float64() * 0.05,
		costA:         r.Float64() * 0.05,
		costB:         r.Float64() * 0.10, // escalation usually costs more
		escalate:      r.Intn(2) == 0,
	})
}

func TestProperty6_EconomiaLiquidaNuncaNegativa(t *testing.T) {
	f := func(g genEsc) bool {
		total := g.costA
		if g.escalate {
			total += g.costB
		}
		saved, floored := NetSavings(g.requestedCost, total)
		if saved < 0 {
			return false // floor of zero (Req 14.9)
		}
		if total >= g.requestedCost {
			// Spent as much or more than the requested model: zero savings AND flagged.
			return saved == 0 && floored
		}
		// Spent less: the savings is exactly the difference, with no floor.
		return !floored && saved == g.requestedCost-total
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// The concrete case the ledger must get right: economy mode fails, escalates, and the
// two attempts together cost MORE than the model the customer asked for.
func TestNetSavings_EscalonamentoCaroNaoDeclaraEconomia(t *testing.T) {
	// The requested model would cost 0.010; cheap 0.002 + strong 0.015 = 0.017.
	saved, floored := NetSavings(0.010, 0.017)
	if saved != 0 || !floored {
		t.Errorf("NetSavings = (%v, %v), expected (0, true)", saved, floored)
	}
}
