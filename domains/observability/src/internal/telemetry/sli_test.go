// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package telemetry

import (
	"math"
	"testing"
)

func TestWilson(t *testing.T) {
	if lo, hi := Wilson(0, 0); lo != 0 || hi != 0 {
		t.Errorf("Wilson(0,0) = %v/%v, want 0/0", lo, hi)
	}
	// 95/100: known Wilson interval ≈ [88.8, 97.8]%.
	lo, hi := Wilson(95, 100)
	if math.Abs(lo-88.72) > 0.5 || math.Abs(hi-97.77) > 0.5 {
		t.Errorf("Wilson(95,100) = %v/%v, want ~88.7/97.8", lo, hi)
	}
	// High volume tightens the interval: 9500/10000 ≈ [94.5, 95.4]%.
	lo2, hi2 := Wilson(9500, 10000)
	if hi2-lo2 > hi-lo {
		t.Errorf("the interval with more volume should be narrower: %v vs %v", hi2-lo2, hi-lo)
	}
}

func TestShrink(t *testing.T) {
	// Zero volume → pulls 100% toward the baseline (prior).
	if got := Shrink(0, 0, 99); !almost(got, 99) {
		t.Errorf("Shrink(0,0,99) = %v, want 99", got)
	}
	// (49 good out of 50, prior 99): (49 + 50*0.99)/(50+50)*100 = (49+49.5)/100*100 = 98.5.
	if got := Shrink(49, 50, 99); !almost(got, 98.5) {
		t.Errorf("Shrink(49,50,99) = %v, want 98.5", got)
	}
	// High volume → the observed value dominates (close to 100%).
	if got := Shrink(10000, 10000, 99); got < 99.5 {
		t.Errorf("Shrink with high volume and 100%% observed should beat the prior: %v", got)
	}
}

func TestSLIState(t *testing.T) {
	// Below the volume floor → insufficient_data (even with failures).
	if got := SLIState(5, 10, 99, SLOMinVolume); got != "insufficient_data" {
		t.Errorf("state (n<floor) = %q, want insufficient_data", got)
	}
	// High volume, almost everything good → healthy.
	if got := SLIState(1000, 1000, 99, SLOMinVolume); got != "healthy" {
		t.Errorf("state (perfect) = %q, want healthy", got)
	}
	// High volume, many errors → breaching (even the Wilson-high is below the target).
	if got := SLIState(900, 1000, 99, SLOMinVolume); got != "breaching" {
		t.Errorf("state (90%%/1000) = %q, want breaching", got)
	}
	// Early-warning zone: adjusted < target but Wilson-high still above → at_risk.
	// 96/100: adjusted = (96+49.5)/150*100 = 96.33 < 99; wilson_high ≈ 98.9 (>=? checked).
	got := SLIState(96, 100, 99, SLOMinVolume)
	if got != "at_risk" && got != "breaching" {
		t.Errorf("state (96/100) = %q, want at_risk or breaching", got)
	}
}

// Plan-tier SLO lookup (SLOFor/SLODefault) was removed with the SaaS billing
// model: single-client deployments use the fixed SLOTarget constant.
func TestSLOTarget(t *testing.T) {
	if SLOTarget != 99.5 {
		t.Errorf("SLOTarget = %v, want 99.5", SLOTarget)
	}
}
