// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package telemetry

import "testing"

func TestBurnRate(t *testing.T) {
	cases := []struct {
		errs, total int
		slo, want   float64
	}{
		{0, 0, 99, 0},
		{10, 100, 99, 10.0},
		{1, 100, 99.9, 10.0},
		{1, 100, 100, 100.0},
		{5, 100, 99, 5.0},
	}
	for i, c := range cases {
		if got := BurnRate(c.errs, c.total, c.slo); !almost(got, c.want) {
			t.Errorf("case %d BurnRate(%d,%d,%v)=%v, want %v", i, c.errs, c.total, c.slo, got, c.want)
		}
	}
}

func TestEvalBurn(t *testing.T) {
	if sev, burn, win := EvalBurn(15, 100, 0, 0, 99); sev != "page" || win != "1h" || !almost(burn, 15.0) {
		t.Errorf("fast burn = %q/%v/%q, want page/15/1h", sev, burn, win)
	}
	if sev, burn, win := EvalBurn(0, 5, 7, 100, 99); sev != "ticket" || win != "6h" || !almost(burn, 7.0) {
		t.Errorf("slow burn = %q/%v/%q, want ticket/7/6h", sev, burn, win)
	}
	if sev, burn, win := EvalBurn(1, 100, 1, 100, 99); sev != "" || win != "" || burn != 0 {
		t.Errorf("no burn = %q/%v/%q, want empty", sev, burn, win)
	}
	if sev, _, _ := EvalBurn(9, 9, 0, 0, 99); sev == "page" {
		t.Errorf("t1<10 should not fire page")
	}
	if sev, _, win := EvalBurn(15, 100, 7, 100, 99); sev != "page" || win != "1h" {
		t.Errorf("precedence = %q/%q, want page/1h", sev, win)
	}
}
