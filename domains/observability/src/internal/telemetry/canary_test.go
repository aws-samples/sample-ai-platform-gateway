// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Canary comparison guards (task 16).
package telemetry

import "testing"

func canaryRecs(n int, model string, canary bool, cost float64, lat int, errs int) []Record {
	out := make([]Record, 0, n)
	for i := 0; i < n; i++ {
		r := Record{Feature: "resumo", Model: model, Cost: cost, Latency: lat,
			Canary: canary, TS: "2026-08-14T10:00:00Z", Status: "success"}
		if canary {
			r.CanaryRoute = model
		}
		if i < errs {
			r.Status, r.SLIEligible, r.FailReason = "error", true, "provider_down"
		}
		out = append(out, r)
	}
	return out
}

// A tiny candidate sample must never read as a result. This is the guard that keeps
// "11 requests looked great" out of a decision to move a production flow.
func TestCompareCanary_SmallSampleIsInconclusive(t *testing.T) {
	recs := append(canaryRecs(500, "sonnet", false, 0.01, 900, 5),
		canaryRecs(11, "haiku", true, 0.001, 300, 0)...)
	cmp := CompareCanary(recs, "resumo", "haiku")

	if cmp.Candidate.Requests != 11 || cmp.Reference.Requests != 500 {
		t.Fatalf("arms split wrong: ref=%d cand=%d", cmp.Reference.Requests, cmp.Candidate.Requests)
	}
	if cmp.Conclusive {
		t.Error("11 requests against 500 must not be conclusive")
	}
	// Cost is measured, not inferred, so it is still reported.
	if cmp.CostDeltaPct >= 0 {
		t.Errorf("cost delta = %v, want negative (candidate is cheaper)", cmp.CostDeltaPct)
	}
}

// With volume on both sides and clearly separated error rates, the comparison may
// call it — but only about errors, cost and latency.
func TestCompareCanary_SeparatedRatesAreConclusive(t *testing.T) {
	recs := append(canaryRecs(400, "sonnet", false, 0.01, 900, 0),
		canaryRecs(400, "haiku", true, 0.001, 300, 120)...)
	cmp := CompareCanary(recs, "resumo", "haiku")

	if !cmp.Conclusive {
		t.Errorf("0%% vs 30%% error over 400 each should be conclusive: ref=[%.2f,%.2f] cand=[%.2f,%.2f]",
			cmp.Reference.SuccessLoPct, cmp.Reference.SuccessHiPct,
			cmp.Candidate.SuccessLoPct, cmp.Candidate.SuccessHiPct)
	}
	if cmp.Candidate.ErrorRatePct <= cmp.Reference.ErrorRatePct {
		t.Errorf("candidate error rate %.2f should exceed reference %.2f",
			cmp.Candidate.ErrorRatePct, cmp.Reference.ErrorRatePct)
	}
	// The caveat travels with the payload so a caller that renders only numbers
	// cannot drop it.
	if cmp.Note == "" {
		t.Error("the quality caveat must travel with the comparison")
	}
}

// Traffic that named the candidate explicitly is NOT experiment traffic. Splitting
// by model name would fold the candidate's organic requests into the sample and the
// comparison would be measuring something else.
func TestCompareCanary_SplitsByMarkNotByModelName(t *testing.T) {
	recs := append(canaryRecs(50, "haiku", false, 0.001, 300, 0), // organic: client asked for haiku
		canaryRecs(30, "haiku", true, 0.001, 300, 0)...) // sampled by the experiment
	cmp := CompareCanary(recs, "resumo", "haiku")

	if cmp.Candidate.Requests != 30 {
		t.Errorf("candidate arm = %d requests, want only the 30 marked ones", cmp.Candidate.Requests)
	}
	if cmp.Reference.Requests != 50 {
		t.Errorf("reference arm = %d requests, want the 50 organic ones", cmp.Reference.Requests)
	}
}

// A period with no experiment at all must produce an empty, inconclusive candidate
// rather than a division by zero or a phantom winner.
func TestCompareCanary_NoExperimentIsEmptyAndInconclusive(t *testing.T) {
	cmp := CompareCanary(canaryRecs(100, "sonnet", false, 0.01, 900, 0), "resumo", "haiku")
	if cmp.Candidate.Requests != 0 || cmp.Conclusive {
		t.Errorf("no canary traffic must be empty and inconclusive: %+v", cmp.Candidate)
	}
	if cmp.CostDeltaPct != -100 {
		t.Errorf("cost delta with an empty candidate = %v; expected -100 (zero cost vs reference)", cmp.CostDeltaPct)
	}
}
