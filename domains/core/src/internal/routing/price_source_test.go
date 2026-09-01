// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import (
	"encoding/json"
	"testing"
	"time"
)

// The provenance survives all three accepted JSON shapes, and the default is `list`.
// The old shape {input,output} — which is what every org has stored today — does not
// have the field, so it MUST fall back to `list`: assuming a contract for legacy data
// would make the platform claim a precision nobody declared.
func TestPriceSourcePorFormaDeJSON(t *testing.T) {
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"old shape without source", `{"input":0.003,"output":0.015}`, PriceSourceList},
		// The console writes in this shape: if the provenance were not read here, every
		// contract price would go back to showing up as a list price.
		{"old shape WITH source", `{"input":0.002,"output":0.01,"source":"contract"}`, PriceSourceContract},
		{"tiers without source", `{"standard":{"input":0.003,"output":0.015}}`, PriceSourceList},
		{"tiers with contract", `{"standard":{"input":0.002,"output":0.01},"source":"contract"}`, PriceSourceContract},
		{"list of validity windows with contract", `[{"effective_from":"2026-01-01","standard":{"input":0.002,"output":0.01},"source":"contract"}]`, PriceSourceContract},
		{"unknown source falls back to list", `{"standard":{"input":0.003,"output":0.015},"source":"chutei"}`, PriceSourceList},
	}
	for _, tc := range tests {
		var h PriceHistory
		if err := json.Unmarshal([]byte(tc.raw), &h); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.name, err)
		}
		tier, st := SelectPrice(h, now)
		if st == PricingUnknown {
			t.Fatalf("%s: the price should not be unknown", tc.name)
		}
		if got := tier.SourceOf(); got != tc.want {
			t.Errorf("%s: SourceOf() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Provenance is METADATA: it must not change a single cent of the computed cost. If it
// did, switching the label would rewrite the customer's cost history.
func TestSourceNaoAlteraCusto(t *testing.T) {
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	parse := func(raw string) PriceTier {
		var h PriceHistory
		if err := json.Unmarshal([]byte(raw), &h); err != nil {
			t.Fatal(err)
		}
		tier, _ := SelectPrice(h, now)
		return tier
	}
	lista := parse(`{"standard":{"input":0.003,"output":0.015}}`)
	contrato := parse(`{"standard":{"input":0.003,"output":0.015},"source":"contract"}`)

	split := InputSplit{Uncached: 1000}
	if a, b := RealizedCost(lista, 0, split, 500), RealizedCost(contrato, 0, split, 500); a != b {
		t.Errorf("cost changed just because of the provenance: %v vs %v", a, b)
	}
	if a, b := ExpectedCost(lista, 0, 1000, 0, 500), ExpectedCost(contrato, 0, 1000, 0, 500); a != b {
		t.Errorf("expected cost changed just because of the provenance: %v vs %v", a, b)
	}
}

// Validity windows may have different provenances: the customer signs a contract from
// a given date and before that the list price applied. The one in force wins.
func TestSourcePorVigencia(t *testing.T) {
	raw := `[
	  {"effective_from":"2026-01-01","standard":{"input":0.003,"output":0.015}},
	  {"effective_from":"2026-06-01","standard":{"input":0.002,"output":0.01},"source":"contract"}
	]`
	var h PriceHistory
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		t.Fatal(err)
	}
	antes, _ := SelectPrice(h, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if got := antes.SourceOf(); got != PriceSourceList {
		t.Errorf("before the contract: %q, want list", got)
	}
	depois, _ := SelectPrice(h, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if got := depois.SourceOf(); got != PriceSourceContract {
		t.Errorf("after the contract: %q, want contract", got)
	}
}
