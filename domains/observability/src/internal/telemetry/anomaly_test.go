// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package telemetry

import (
	"math"
	"testing"
)

func TestAnomalyZ(t *testing.T) {
	if got := AnomalyZ(0, 0, 0.5); got != 0 {
		t.Errorf("AnomalyZ n=0 = %v, quero 0", got)
	}
	if got := AnomalyZ(10, 100, 0.01); math.Abs(got-9.045340337332909) > 1e-6 {
		t.Errorf("AnomalyZ(10,100,0.01) = %v, quero ~9.0453", got)
	}
	if got := AnomalyZ(12, 100, 0.1); math.Abs(got-0.6666666666666666) > 1e-9 {
		t.Errorf("AnomalyZ(12,100,0.1) = %v, want ~0.6667", got)
	}
	// a baseline of ~0 uses the 0.0001 floor so an isolated error does not blow up the z.
	if got := AnomalyZ(1, 100, 0); math.IsInf(got, 0) || math.IsNaN(got) {
		t.Errorf("AnomalyZ with a baseline of 0 should not be Inf/NaN: %v", got)
	}
}

func TestEvalAnomaly(t *testing.T) {
	z, cur, base, fire := EvalAnomaly(5, 500, 10, 100)
	if !fire || !almost(cur, 0.10) || !almost(base, 0.01) {
		t.Errorf("expected it to fire with cur/base 0.10/0.01; z=%v cur=%v base=%v fire=%v", z, cur, base, fire)
	}
	if math.Abs(z-9.045340337332909) > 1e-6 {
		t.Errorf("z = %v, want ~9.0453", z)
	}
	if _, _, _, fire := EvalAnomaly(5, 40, 10, 100); fire {
		t.Errorf("a baseline <50 should not fire")
	}
	if _, _, _, fire := EvalAnomaly(5, 500, 2, 8); fire {
		t.Errorf("a current volume <10 should not fire")
	}
	if _, _, _, fire := EvalAnomaly(50, 500, 12, 100); fire {
		t.Errorf("within the normal range (z<3) should not fire")
	}
}
