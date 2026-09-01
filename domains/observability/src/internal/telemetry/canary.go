// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Canary comparison — PURE DOMAIN.
//
// A canary sends a slice of a feature's traffic to a candidate route so the two can
// be compared on the customer's OWN prompts. This file computes that comparison and
// is deliberately narrow about what it will say.
//
// What it reports: cost per request, average latency, error rate — each with the
// volume floor and the Wilson interval already used by the SLO, so a candidate that
// served eleven requests cannot look "better" than a reference that served eleven
// thousand.
//
// What it refuses to report: any conclusion about QUALITY. Response depth is not
// observable from tokens, money and milliseconds. Stating "the candidate is as good"
// would be the single most damaging false claim this product could make, because it
// is exactly the claim a customer with a tuned reasoning flow wants to hear.
package telemetry

// CanarySide is one arm of the experiment.
type CanarySide struct {
	Route         string  `json:"route"`
	Requests      int     `json:"requests"`
	CostUSD       float64 `json:"cost_usd"`
	CostPerReqUSD float64 `json:"cost_per_request_usd"`
	AvgLatencyMs  int     `json:"avg_latency_ms"`
	Errors        int     `json:"errors"`
	Eligible      int     `json:"eligible"`
	ErrorRatePct  float64 `json:"error_rate_pct"`
	// SuccessLoPct/SuccessHiPct are the Wilson bounds on the success rate. They are
	// the honest way to answer "is this difference real?" with small samples: two
	// overlapping intervals mean the data does not separate the arms yet.
	SuccessLoPct float64 `json:"success_lo_pct"`
	SuccessHiPct float64 `json:"success_hi_pct"`
}

// CanaryComparison is the pair plus the guards on reading it.
type CanaryComparison struct {
	Feature   string     `json:"feature"`
	Reference CanarySide `json:"reference"`
	Candidate CanarySide `json:"candidate"`
	// Conclusive is false when either arm is under the volume floor or the success
	// intervals overlap. The UI must not present a winner when this is false.
	Conclusive bool `json:"conclusive"`
	// CostDeltaPct is the candidate's cost per request against the reference's,
	// negative meaning cheaper. Reported even when inconclusive, because cost is
	// measured rather than inferred — only the error-rate verdict needs the guard.
	CostDeltaPct float64 `json:"cost_delta_pct"`
	// Note travels with the payload so the caveat cannot be dropped by a caller
	// that only renders the numbers.
	Note string `json:"note"`
}

// sideOf accumulates one arm.
func sideOf(route string, recs []Record) CanarySide {
	s := CanarySide{Route: route}
	latSum := 0
	for _, r := range recs {
		if !r.IsSync() {
			continue
		}
		isSuccess := r.Status == "" || r.Status == "success"
		// Same eligibility rule as the SLI: a request blocked by policy or by the
		// customer's own config is not a failure of the route being tested, and
		// counting it would make the arm with more policy blocks look worse.
		if isSuccess || r.SLIEligible {
			s.Eligible++
			if !isSuccess {
				s.Errors++
			}
		}
		s.Requests++
		s.CostUSD += r.Cost
		latSum += r.Latency
	}
	if s.Requests > 0 {
		s.CostPerReqUSD = s.CostUSD / float64(s.Requests)
		s.AvgLatencyMs = latSum / s.Requests
	}
	if s.Eligible > 0 {
		s.ErrorRatePct = float64(s.Errors) / float64(s.Eligible) * 100
		s.SuccessLoPct, s.SuccessHiPct = Wilson(s.Eligible-s.Errors, s.Eligible)
	}
	return s
}

// CompareCanary splits a feature's records into reference and candidate arms and
// computes the comparison.
//
// The split is by the CANARY MARK, not by model name. Using the name would put the
// candidate's own organic traffic (a request that named it explicitly) inside the
// experiment, and the experiment would then be measuring something else.
func CompareCanary(recs []Record, feature, candidateRoute string) CanaryComparison {
	var refRecs, candRecs []Record
	refRoute := ""
	for _, r := range recs {
		if feature != "" && r.Feature != feature {
			continue
		}
		if r.Canary && (candidateRoute == "" || r.CanaryRoute == candidateRoute || r.Model == candidateRoute) {
			candRecs = append(candRecs, r)
			continue
		}
		if r.Canary {
			continue // sampled by a DIFFERENT experiment; belongs to neither arm here
		}
		if refRoute == "" {
			refRoute = r.Model
		}
		refRecs = append(refRecs, r)
	}

	cmp := CanaryComparison{
		Feature:   feature,
		Reference: sideOf(refRoute, refRecs),
		Candidate: sideOf(candidateRoute, candRecs),
		Note:      "compares cost, latency and errors only; response quality is not measured",
	}
	if cmp.Reference.CostPerReqUSD > 0 {
		cmp.CostDeltaPct = (cmp.Candidate.CostPerReqUSD - cmp.Reference.CostPerReqUSD) / cmp.Reference.CostPerReqUSD * 100
	}
	// Both arms above the floor AND non-overlapping success intervals. Overlap means
	// the sample cannot tell the arms apart yet, and saying otherwise would dress up
	// noise as a result.
	if cmp.Reference.Eligible >= SLOMinVolume && cmp.Candidate.Eligible >= SLOMinVolume {
		a, b := cmp.Reference, cmp.Candidate
		cmp.Conclusive = a.SuccessLoPct > b.SuccessHiPct || b.SuccessLoPct > a.SuccessHiPct
	}
	return cmp
}
