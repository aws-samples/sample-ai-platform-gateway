// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package telemetry

import "math"

// Anomaly detection by binomial z-score against the CUSTOMER'S OWN baseline.
// A faithful move of what used to live in cmd/alert-notifier/main.go — no rule
// rewritten.

// AnomalyZ: z-score of the current window's error count under the customer's BASELINE
// rate (binomial). It measures "how far outside their own normal" they are — catching
// a spike even inside the global SLO. z>=3 ≈ 99.7% confidence of being above the
// baseline.
func AnomalyZ(obsErrs, n int, pBase float64) float64 {
	if n == 0 {
		return 0
	}
	if pBase <= 0 {
		pBase = 0.0001 // floor so a baseline of ~0 does not blow up the z on 1 error
	}
	mean := float64(n) * pBase
	sd := math.Sqrt(float64(n) * pBase * (1 - pBase))
	if sd == 0 {
		return 0
	}
	return (float64(obsErrs) - mean) / sd
}

// EvalAnomaly decides whether the last hour's error rate is an anomaly vs. the
// customer's own baseline. Pure. Anti-noise guards: a reliable baseline (>=50
// eligible) + current volume >=10 + z>=3 + >=2x the baseline + a 2% floor + >=3
// errors. Returns the z-score, the current rate, the baseline and whether it fires.
func EvalAnomaly(be, bt, ce, ct int) (z, curRate, baseRate float64, fire bool) {
	if bt < 50 || ct < 10 {
		return 0, 0, 0, false
	}
	baseRate = float64(be) / float64(bt)
	curRate = float64(ce) / float64(ct)
	z = AnomalyZ(ce, ct, baseRate)
	fire = z >= 3 && curRate >= math.Max(2*baseRate, 0.02) && ce >= 3
	return z, curRate, baseRate, fire
}
