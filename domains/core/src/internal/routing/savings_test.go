// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import (
	"math"
	"testing"
	"testing/quick"
)

func TestClassOf(t *testing.T) {
	cases := map[string]string{
		ReasonResponseCache:       SavingsVerified,
		ReasonProviderPromptCache: SavingsVerified,
		ReasonAutoCheapest:        SavingsCounterfactual,
		ReasonFallback:            SavingsCounterfactual,
		ReasonBudgetDegrade:       SavingsCounterfactual,
		"":                        "",
		// A new reason nobody has classified yet: falls into counterfactual, the
		// LEAST defensible class. It is the safe default for the invoice.
		"motivo_que_nao_existe": SavingsCounterfactual,
	}
	for reason, want := range cases {
		if got := ClassOf(reason); got != want {
			t.Errorf("ClassOf(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestSplitSavingsCasosConhecidos(t *testing.T) {
	tests := []struct {
		name              string
		total, verifiedIn float64
		wantV, wantC      float64
	}{
		{"swap only", 10, 0, 0, 10},
		{"cache only", 10, 10, 10, 0},
		{"mixed", 10, 3, 3, 7},
		{"zero total", 0, 5, 0, 0},
		{"negative total never gets in", -4, 0, 0, 0},
		// Inconsistent provider: cache share larger than the total. We cap the
		// verified part at the total instead of letting the counterfactual go negative.
		{"share larger than the total", 5, 9, 5, 0},
		{"negative share", 5, -2, 0, 5},
	}
	for _, tc := range tests {
		v, c := SplitSavings(tc.total, tc.verifiedIn)
		if v != tc.wantV || c != tc.wantC {
			t.Errorf("%s: SplitSavings(%v,%v) = (%v,%v), want (%v,%v)",
				tc.name, tc.total, tc.verifiedIn, v, c, tc.wantV, tc.wantC)
		}
	}
}

// Property: the partition closes and no part is negative, for any input. That is what
// guarantees adding the ledger's two columns gives back the total shown to the
// customer — without it, the screen's three lines do not reconcile.
func TestPropSplitSavingsParticiona(t *testing.T) {
	f := func(total, portion float64) bool {
		if math.IsNaN(total) || math.IsInf(total, 0) || math.IsNaN(portion) || math.IsInf(portion, 0) {
			return true
		}
		v, c := SplitSavings(total, portion)
		if v < 0 || c < 0 {
			return false
		}
		if total <= 0 {
			return v == 0 && c == 0
		}
		// RELATIVE tolerance: at magnitudes around 1e308 a float64's absolute error is
		// enormous by construction, and comparing against 1e-9 would fail correct
		// arithmetic. What matters is the partition closing at the value's scale.
		return math.Abs((v+c)-total) <= 1e-12*math.Abs(total)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}
