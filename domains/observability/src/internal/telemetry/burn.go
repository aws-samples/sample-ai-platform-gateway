// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package telemetry

// Multi-window burn rate (SRE workbook). A faithful move of what used to live in
// cmd/alert-notifier/main.go — no rule rewritten.

// BurnRate: how fast the error budget is being consumed, relative to consuming it
// "on pace" (burn=1 spends the budget exactly at the end of the SLO window). It is
// the observed error rate divided by the error budget (1-SLO). It only alerts when
// the BUDGET is actually threatened (not on an isolated error).
func BurnRate(errs, total int, sloPct float64) float64 {
	if total == 0 {
		return 0
	}
	budget := 1 - sloPct/100 // tolerated error fraction
	if budget <= 0 {
		budget = 0.0001
	}
	return (float64(errs) / float64(total)) / budget
}

// EvalBurn decides the severity of the multi-window burn rate alert from the
// (errors,total) counts of the 1h and 6h windows plus the tier's SLO. Pure. Thresholds
// from the SRE workbook: fast burn 14.4x/1h (page) with a volume floor of 10; slow
// 6x/6h (ticket) with a floor of 20. Returns an empty sev when no window fires.
func EvalBurn(e1, t1, e6, t6 int, slo float64) (sev string, burn float64, win string) {
	b1, b6 := BurnRate(e1, t1, slo), BurnRate(e6, t6, slo)
	if t1 >= 10 && b1 >= 14.4 {
		return "page", b1, "1h"
	}
	if t6 >= 20 && b6 >= 6 {
		return "ticket", b6, "6h"
	}
	return "", 0.0, ""
}
