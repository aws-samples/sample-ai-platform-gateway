// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Tests for model identity, swap classification and swap policy.
//
// The truth tables are validated against the shared fixture at
// testdata/contracts/model-identity-bundles/identity-cases.json, so the Core and
// the Governance agree on the vocabulary without importing each other.
package routing

import (
	"encoding/json"
	"os"
	"testing"
)

// From this package directory to the repository root is 5 levels.
const identityFixturePath = "../../../../../testdata/contracts/model-identity-bundles/identity-cases.json"

type identityFixture struct {
	Routes map[string]struct {
		Provider   string `json:"provider"`
		ModelID    string `json:"model_id"`
		Aggregator bool   `json:"aggregator"`
		Caps       struct {
			Tier    string `json:"tier"`
			ToolUse bool   `json:"tool_use"`
			Multi   bool   `json:"multimodal"`
			CtxWin  int    `json:"context_window_tokens"`
		} `json:"capabilities"`
		Price struct {
			Input  float64 `json:"input"`
			Output float64 `json:"output"`
		} `json:"price"`
	} `json:"routing"`
	SwapCases []struct {
		Requested string `json:"pedida"`
		Served    string `json:"servida"`
		Class     string `json:"classe"`
		Why       string `json:"porque"`
	} `json:"classificacao_de_troca"`
	PolicyCases []struct {
		Policy  string `json:"politica"`
		Class   string `json:"classe"`
		Allowed bool   `json:"permitido"`
		Why     string `json:"porque"`
	} `json:"politica_de_troca"`
	PinCases []struct {
		Pins     []string `json:"feature_models"`
		Eligible []string `json:"elegiveis"`
		Why      string   `json:"porque"`
	} `json:"pin_por_modelo"`
}

func loadIdentityFixture(t *testing.T) identityFixture {
	t.Helper()
	b, err := os.ReadFile(identityFixturePath)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var f identityFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	if len(f.Routes) == 0 || len(f.SwapCases) == 0 {
		t.Fatal("empty fixture validates nothing")
	}
	return f
}

// fixtureCandidates turns the fixture catalog into domain candidates.
func fixtureCandidates(t *testing.T, f identityFixture) []Candidate {
	t.Helper()
	out := make([]Candidate, 0, len(f.Routes))
	for name, r := range f.Routes {
		out = append(out, Candidate{
			Model: name, Provider: r.Provider,
			ModelID: r.ModelID, Aggregator: r.Aggregator,
			Caps: Capabilities{
				Tier: r.Caps.Tier, ToolUse: r.Caps.ToolUse,
				Multimodal: r.Caps.Multi, ContextWindow: r.Caps.CtxWin,
			},
			Prices: flatPrice(r.Price.Input, r.Price.Output),
		})
	}
	return out
}

func candByName(cands []Candidate, name string) Candidate {
	for _, c := range cands {
		if c.Model == name {
			return c
		}
	}
	return Candidate{}
}

// --- Identity grouping -------------------------------------------------------

func TestBuildIdentity_GroupsByDeclaredModelID(t *testing.T) {
	f := loadIdentityFixture(t)
	id := BuildIdentity(fixtureCandidates(t, f))

	group := id.RoutesFor("openai/gpt-5.2")
	got := map[string]bool{}
	for _, r := range group {
		got[r] = true
	}
	if !got["gpt-openai"] || !got["gpt-azure"] {
		t.Errorf("group for openai/gpt-5.2 = %v, want gpt-openai and gpt-azure", group)
	}
	if got["gpt-via-agregador"] {
		t.Error("an aggregator must never join an identity group")
	}
	if len(group) != 2 {
		t.Errorf("group size = %d, want 2", len(group))
	}
}

// Identity is never inferred: two routes with the SAME provider_model_id and no
// declared model_id stay in separate groups. Inferring would produce a silent
// false positive exactly where the damage is worst.
func TestBuildIdentity_NeverInfersFromProviderModelID(t *testing.T) {
	f := loadIdentityFixture(t)
	cands := fixtureCandidates(t, f)
	id := BuildIdentity(cands)

	a := candByName(cands, "sem-identidade-a")
	b := candByName(cands, "sem-identidade-b")
	if a.Model == "" || b.Model == "" {
		t.Fatal("fixture must contain the two undeclared routes")
	}
	if id.SameModel(a, b) {
		t.Error("routes without model_id must never be the same model, even sharing provider_model_id")
	}
	if g := id.GroupOf("sem-identidade-a"); len(g) != 1 || g[0] != "sem-identidade-a" {
		t.Errorf("undeclared route group = %v, want itself only", g)
	}
}

// An aggregator may serve a different version or quantization between requests,
// so it stays alone even when it declares a shared model_id.
func TestBuildIdentity_AggregatorStaysAlone(t *testing.T) {
	f := loadIdentityFixture(t)
	cands := fixtureCandidates(t, f)
	id := BuildIdentity(cands)

	agg := candByName(cands, "gpt-via-agregador")
	direct := candByName(cands, "gpt-openai")
	if agg.ModelID != direct.ModelID || agg.ModelID == "" {
		t.Fatal("fixture must have the aggregator declaring the same model_id")
	}
	if id.SameModel(agg, direct) {
		t.Error("aggregator must not be treated as the same model")
	}
	if g := id.GroupOf("gpt-via-agregador"); len(g) != 1 {
		t.Errorf("aggregator group = %v, want a group of one", g)
	}
}

// --- Swap classification (fixture truth table) ------------------------------

func TestSwapClassOf_MatchesFixture(t *testing.T) {
	f := loadIdentityFixture(t)
	cands := fixtureCandidates(t, f)
	id := BuildIdentity(cands)

	for _, tc := range f.SwapCases {
		t.Run(tc.Requested+" -> "+tc.Served, func(t *testing.T) {
			req := candByName(cands, tc.Requested)
			srv := candByName(cands, tc.Served)
			if req.Model == "" || srv.Model == "" {
				t.Fatalf("fixture references unknown route: %q or %q", tc.Requested, tc.Served)
			}
			if got := id.SwapClassOf(req, srv); got != tc.Class {
				t.Errorf("class = %q, fixture says %q (%s)", got, tc.Class, tc.Why)
			}
		})
	}
}

// The classification must be total: exactly one value, and SwapNone if and only if
// the routes are the same.
func TestSwapClassOf_IsTotalAndExclusive(t *testing.T) {
	f := loadIdentityFixture(t)
	cands := fixtureCandidates(t, f)
	id := BuildIdentity(cands)
	valid := map[string]bool{SwapNone: true, SwapSameModel: true, SwapEquivalent: true, SwapDowngrade: true}

	for _, a := range cands {
		for _, b := range cands {
			got := id.SwapClassOf(a, b)
			if !valid[got] {
				t.Fatalf("SwapClassOf(%s,%s) = %q, outside the class set", a.Model, b.Model, got)
			}
			if (got == SwapNone) != (a.Model == b.Model) {
				t.Errorf("SwapClassOf(%s,%s) = %q; SwapNone must mean same route", a.Model, b.Model, got)
			}
		}
	}
}

// --- Swap policy (fixture truth table) -------------------------------------

func TestSwapAllowed_MatchesFixture(t *testing.T) {
	f := loadIdentityFixture(t)
	for _, tc := range f.PolicyCases {
		name := tc.Policy + "/" + tc.Class
		if tc.Policy == "" {
			name = "absent/" + tc.Class
		}
		t.Run(name, func(t *testing.T) {
			if got := SwapAllowed(tc.Policy, tc.Class); got != tc.Allowed {
				t.Errorf("SwapAllowed(%q,%q) = %v, fixture says %v (%s)",
					tc.Policy, tc.Class, got, tc.Allowed, tc.Why)
			}
		})
	}
}

// An unknown policy value must permit everything: an unreadable policy cannot
// start silently refusing traffic.
func TestSwapAllowed_UnknownPolicyPermitsAll(t *testing.T) {
	for _, class := range []string{SwapNone, SwapSameModel, SwapEquivalent, SwapDowngrade} {
		if !SwapAllowed("something-new-from-the-future", class) {
			t.Errorf("unknown policy must permit %q", class)
		}
	}
}

// --- Pin by model id (fixture truth table) ---------------------------------

func TestPin_MatchesFixture(t *testing.T) {
	f := loadIdentityFixture(t)
	cands := fixtureCandidates(t, f)
	id := BuildIdentity(cands)

	for _, tc := range f.PinCases {
		t.Run(tc.Why, func(t *testing.T) {
			want := map[string]bool{}
			for _, m := range tc.Eligible {
				want[m] = true
			}
			for _, c := range cands {
				got := pinMatches(tc.Pins, c, id)
				if got != want[c.Model] {
					t.Errorf("route %q eligible = %v, fixture says %v", c.Model, got, want[c.Model])
				}
			}
		})
	}
}

// Ambiguity rule: a value that is both a route name and a model id reads as the
// ROUTE name, because a route name points at exactly one route while a model id
// opens a set. When in doubt, restrict.
func TestPin_AmbiguousValueResolvesAsRouteName(t *testing.T) {
	cands := []Candidate{
		{Model: "openai/gpt-5.2", ModelID: "openai/gpt-5.2", Caps: Capabilities{Tier: "frontier"}},
		{Model: "gpt-elsewhere", ModelID: "openai/gpt-5.2", Caps: Capabilities{Tier: "frontier"}},
	}
	id := BuildIdentity(cands)
	pins := []string{"openai/gpt-5.2"}

	if !pinMatches(pins, cands[0], id) {
		t.Error("the route literally named like the model id must match")
	}
	if pinMatches(pins, cands[1], id) {
		t.Error("the ambiguous pin must NOT widen to the whole identity group")
	}
}

func TestPin_EmptyListAllowsEverything(t *testing.T) {
	f := loadIdentityFixture(t)
	cands := fixtureCandidates(t, f)
	id := BuildIdentity(cands)
	for _, c := range cands {
		if !pinMatches(nil, c, id) {
			t.Errorf("empty pin list must allow %q", c.Model)
		}
	}
}

// --- Ledger classification -------------------------------------------------

func TestClassOf_ArbitrageIsVerified(t *testing.T) {
	if got := ClassOf(ReasonProviderArbitrage); got != SavingsVerified {
		t.Errorf("ClassOf(provider_arbitrage) = %q, want %q — the model served is the model that would be served",
			got, SavingsVerified)
	}
}

func TestClassOf_ModelSwapStaysCounterfactual(t *testing.T) {
	for _, r := range []string{ReasonAutoCheapest, ReasonFallback, ReasonBudgetDegrade, ReasonSemanticCache} {
		if got := ClassOf(r); got != SavingsCounterfactual {
			t.Errorf("ClassOf(%q) = %q, want counterfactual", r, got)
		}
	}
}
