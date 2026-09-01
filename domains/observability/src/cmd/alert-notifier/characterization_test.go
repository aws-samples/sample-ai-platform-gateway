// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// CHARACTERIZATION test of the alert-notifier (hexagonal-refactor, task 15.3).
//
// Captures the CURRENT behavior of the reliability math — multi-window burn rate and
// anomaly detection by z-score — BEFORE any move to internal/telemetry. After the
// migration, these same functions become aliases (`var fn = telemetry.Fn`), so the
// test stays valid and proves the behavior did not change (D6). If it reports a
// difference, the difference is a defect of the refactor — fix the code, not the golden.
//
// Runs offline: package main, without touching DynamoDB.
package main

import (
	"math"
	"testing"
)

const eps = 1e-9

func almost(a, b float64) bool { return math.Abs(a-b) <= eps }

func TestChar_BurnRate(t *testing.T) {
	cases := []struct {
		errs, total int
		slo, want   float64
	}{
		{0, 0, 99, 0},        // no traffic → no burn
		{10, 100, 99, 10.0},  // 10% error rate with a 1% budget → 10x
		{1, 100, 99.9, 10.0}, // 1% error rate with a 0.1% budget → 10x
		{1, 100, 100, 100.0}, // SLO 100% → budget floor 0.0001 → does not blow up into NaN
		{5, 100, 99, 5.0},
	}
	for i, c := range cases {
		if got := burnRate(c.errs, c.total, c.slo); !almost(got, c.want) {
			t.Errorf("case %d burnRate(%d,%d,%v)=%v, want %v", i, c.errs, c.total, c.slo, got, c.want)
		}
	}
}

func TestChar_EvalBurn(t *testing.T) {
	// fast burn: 15% over 1h with t1>=10 and b1>=14.4 → page.
	if sev, burn, win := evalBurn(15, 100, 0, 0, 99); sev != "page" || win != "1h" || !almost(burn, 15.0) {
		t.Errorf("fast burn = %q/%v/%q, want page/15/1h", sev, burn, win)
	}
	// slow burn: 1h without volume, 6h with 7% and t6>=20 and b6>=6 → ticket.
	if sev, burn, win := evalBurn(0, 5, 7, 100, 99); sev != "ticket" || win != "6h" || !almost(burn, 7.0) {
		t.Errorf("slow burn = %q/%v/%q, want ticket/7/6h", sev, burn, win)
	}
	// no window fires.
	if sev, burn, win := evalBurn(1, 100, 1, 100, 99); sev != "" || win != "" || burn != 0 {
		t.Errorf("no burn = %q/%v/%q, want empty", sev, burn, win)
	}
	// volume floor: 1h with a high b1 but t1<10 → does not page.
	if sev, _, _ := evalBurn(9, 9, 0, 0, 99); sev == "page" {
		t.Errorf("volume below the floor should not fire page (t1<10)")
	}
	// precedence: when both fire, page (1h) wins.
	if sev, _, win := evalBurn(15, 100, 7, 100, 99); sev != "page" || win != "1h" {
		t.Errorf("precedence = %q/%q, want page/1h", sev, win)
	}
}

func TestChar_AnomalyZ(t *testing.T) {
	if got := anomalyZ(0, 0, 0.5); got != 0 {
		t.Errorf("anomalyZ n=0 = %v, want 0", got)
	}
	// (10 errors in 100, baseline 1%): mean=1, sd=sqrt(0.99), z≈9.045.
	if got := anomalyZ(10, 100, 0.01); math.Abs(got-9.045340337332909) > 1e-6 {
		t.Errorf("anomalyZ(10,100,0.01) = %v, want ~9.0453", got)
	}
	// (12 errors in 100, baseline 10%): mean=10, sd=3, z≈0.667.
	if got := anomalyZ(12, 100, 0.1); math.Abs(got-0.6666666666666666) > 1e-9 {
		t.Errorf("anomalyZ(12,100,0.1) = %v, want ~0.6667", got)
	}
}

func TestChar_EvalAnomaly(t *testing.T) {
	// FIRES: reliable baseline (1% in 500), current 10% in 100, high z.
	z, cur, base, fire := evalAnomaly(5, 500, 10, 100)
	if !fire {
		t.Errorf("expected it to fire; z=%v cur=%v base=%v", z, cur, base)
	}
	if !almost(cur, 0.10) || !almost(base, 0.01) {
		t.Errorf("cur/base = %v/%v, want 0.10/0.01", cur, base)
	}
	if math.Abs(z-9.045340337332909) > 1e-6 {
		t.Errorf("z = %v, want ~9.0453", z)
	}
	// Does NOT fire: baseline with a small sample (bt<50).
	if _, _, _, fire := evalAnomaly(5, 40, 10, 100); fire {
		t.Errorf("a baseline below 50 should not fire")
	}
	// Does NOT fire: small current volume (ct<10).
	if _, _, _, fire := evalAnomaly(5, 500, 2, 8); fire {
		t.Errorf("a current volume below 10 should not fire")
	}
	// Does NOT fire: within the customer's normal (z<3).
	if _, _, _, fire := evalAnomaly(50, 500, 12, 100); fire {
		t.Errorf("within the normal range (z<3) should not fire")
	}
}
