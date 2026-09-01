// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package telemetry

import "math"

// Volume-sensitive SLO (design R7.1). Fixed target + volume floor + Wilson
// interval (confidence) + Bayesian shrinkage (a fair estimate for low/medium volume)
// + state classification. The pure domain is the natural home for this math (the
// steering says it is exposed in the `sli` of the usage-api).

// SLOMinVolume: below this we do NOT evaluate the SLO (avoids panic on a small
// sample — 1 failure in 2 calls is not a signal, it is noise). Volume floor.
const SLOMinVolume = 20

// SLOPriorStrength (k): strength of the Bayesian anchor, in "pseudo-observations".
// Below ~k eligible requests the estimate is pulled toward the tier baseline; above
// it, the customer's real data dominates. It is what gives the "volume-weighted
// average".
const SLOPriorStrength = 50.0

// SLOTarget: availability target for the deployment (industry standard for a
// production API). Single-client deployments have one SLO, not a tier ladder —
// it may still be overridden per org via config in the future if needed.
const SLOTarget = 99.5

// Shrink returns the success rate ESTIMATED by Bayesian shrinkage (Beta-Binomial):
// it blends the observed value (good/eligible) with a baseline (priorPct), weighted
// by volume. Low volume → close to the baseline (does not freak out over 1 failure);
// high volume → close to the observed. In %.
func Shrink(good, eligible int, priorPct float64) float64 {
	k := SLOPriorStrength
	prior := priorPct / 100
	return (float64(good) + k*prior) / (float64(eligible) + k) * 100
}

// Wilson returns the Wilson confidence interval (95%, z=1.96) of the success rate,
// in %. It is what makes the evaluation sensitive to VOLUME: with few samples the
// interval is wide (we do not trust it); with many, it tightens (we do).
func Wilson(good, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 0
	}
	phat := float64(good) / float64(n)
	z := 1.96
	z2 := z * z
	fn := float64(n)
	denom := 1 + z2/fn
	center := (phat + z2/(2*fn)) / denom
	margin := (z * math.Sqrt(phat*(1-phat)/fn+z2/(4*fn*fn))) / denom
	return (center - margin) * 100, (center + margin) * 100
}

// SLIState classifies the SLO state from the observed value (good/eligible), the
// target (target %) and the volume floor. It follows the same decision order as the
// backoffice:
//
//   - insufficient_data: sample below the floor → we do not evaluate (no panic);
//   - breaching: even the OPTIMISTIC Wilson bound is below the target (confident);
//   - at_risk: adjusted estimate (shrinkage) below the target (early warning);
//   - healthy: otherwise.
func SLIState(good, eligible int, target float64, minVolume int) string {
	switch {
	case eligible < minVolume:
		return "insufficient_data"
	}
	_, wHi := Wilson(good, eligible)
	switch {
	case wHi < target:
		return "breaching"
	case Shrink(good, eligible, target) < target:
		return "at_risk"
	default:
		return "healthy"
	}
}
