// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Canary determinism and distribution (task 14).
package routing

import (
	"strconv"
	"testing"
)

// Reproducibility is the requirement, not a nicety: the customer must be able to
// ask why request X went to the candidate and get an answer recomputable from the
// record.
func TestInCanary_Deterministic(t *testing.T) {
	c := Canary{Route: "claude-haiku", Fraction: 0.5}
	for _, rid := range []string{"req-1", "req-2", "req-abc", ""} {
		first := InCanary(c, rid)
		for i := 0; i < 20; i++ {
			if InCanary(c, rid) != first {
				t.Fatalf("request %q changed sides between evaluations", rid)
			}
		}
	}
}

// An invalid fraction disables the experiment instead of being clamped. Clamping
// 1.5 to 1.0 would route a feature's ENTIRE traffic to a candidate because of a
// typo — the loudest failure from the quietest mistake.
func TestInCanary_InvalidFractionDisables(t *testing.T) {
	for _, f := range []float64{0, -0.1, 1.0001, 2, 100} {
		if InCanary(Canary{Route: "r", Fraction: f}, "req-1") {
			t.Errorf("fraction %v must disable the canary", f)
		}
	}
	// A missing route is equally inert: there is nowhere to send the sample.
	if InCanary(Canary{Fraction: 0.5}, "req-1") {
		t.Error("a canary with no route must be inert")
	}
}

// Fraction 1 means every request, which is the one legitimate way to move a whole
// feature to a candidate.
func TestInCanary_FullFractionTakesEverything(t *testing.T) {
	c := Canary{Route: "r", Fraction: 1}
	for i := 0; i < 200; i++ {
		if !InCanary(c, "req-"+strconv.Itoa(i)) {
			t.Fatalf("request %d excluded at fraction 1", i)
		}
	}
}

// Distribution property: over many identifiers the sample approaches the declared
// share. Tolerance is wide on purpose — this asserts the hash is not biased, not
// that it is a perfect uniform source.
func TestInCanary_DistributionApproachesFraction(t *testing.T) {
	const n = 20000
	for _, frac := range []float64{0.01, 0.05, 0.25, 0.5} {
		hits := 0
		c := Canary{Route: "claude-haiku", Fraction: frac}
		for i := 0; i < n; i++ {
			if InCanary(c, "req-"+strconv.Itoa(i)) {
				hits++
			}
		}
		got := float64(hits) / n
		tol := 0.01 + frac*0.1
		if got < frac-tol || got > frac+tol {
			t.Errorf("fraction %v: sampled %.4f, outside ±%.4f", frac, got, tol)
		}
	}
}

// Two features canarying different candidates must not sample the same identifiers,
// otherwise the experiments correlate and neither result stands alone.
func TestInCanary_RouteNameDecorrelatesExperiments(t *testing.T) {
	a := Canary{Route: "candidate-a", Fraction: 0.3}
	b := Canary{Route: "candidate-b", Fraction: 0.3}
	same, total := 0, 3000
	for i := 0; i < total; i++ {
		rid := "req-" + strconv.Itoa(i)
		if InCanary(a, rid) == InCanary(b, rid) {
			same++
		}
	}
	// Independent 30% splits agree on ~58% of identifiers (0.3²+0.7²). Identical
	// hashing would agree on 100%.
	if same > total*80/100 {
		t.Errorf("the two canaries agreed on %d/%d identifiers — inputs look correlated", same, total)
	}
}
