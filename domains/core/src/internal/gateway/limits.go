// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

// Operational enforcement: rate limit counters per window, monthly spend (budget) and
// credit consumption. They live in the Core's own table (LIMITS_TABLE), with TTL —
// never in Observability's Cost_Store, which belongs to another domain.
//
// They are APPROXIMATE guardrails on purpose: the historical and authoritative source
// of cost is the Cost_Store. Reading the Cost_Store here, on the hot path, would couple
// the two domains through the worst possible door (a synchronous read) to gain
// precision the guardrail does not need.

import (
	"context"
	"time"

	"github.com/aiplat/core/internal/routing"
)

// bump atomically increments a counter and returns the new value (delegates to the
// ddblimits adapter; an unconfigured table = no-op).
func bump(ctx context.Context, pk string, field string, delta float64, ttl time.Time) (float64, error) {
	return limitsStore.Bump(ctx, pk, field, delta, ttl)
}

// readCounter returns a counter's current value (delegates to ddblimits).
func readCounter(ctx context.Context, pk, field string) float64 {
	return limitsStore.Read(ctx, pk, field)
}

// checkRate applies requests/min (atomic count) and tokens/min (after the fact).
// It returns false when the request must be refused.
func checkRate(ctx context.Context, scope string, lim *Limits) bool {
	if lim == nil || scope == "" {
		return true
	}
	win := time.Now().UTC().Format("200601021504") // 1-minute window
	pk := "RATE#" + scope + "#" + win
	exp := time.Now().Add(3 * time.Minute)

	if lim.TPM > 0 {
		// Tokens are only known after the call: bar the NEXT one if it already blew past.
		if readCounter(ctx, pk, "tokens") >= float64(lim.TPM) {
			return false
		}
	}
	if lim.RPM > 0 {
		n, err := bump(ctx, pk, "reqs", 1, exp)
		if err == nil && n > float64(lim.RPM) {
			return false
		}
	}
	return true
}

func addRateTokens(ctx context.Context, scope string, lim *Limits, tokens int) {
	if lim == nil || lim.TPM <= 0 || scope == "" || tokens <= 0 {
		return
	}
	win := time.Now().UTC().Format("200601021504")
	bump(ctx, "RATE#"+scope+"#"+win, "tokens", float64(tokens), time.Now().Add(3*time.Minute))
}

func monthKey(scope string) string {
	return "SPEND#" + scope + "#" + time.Now().UTC().Format("200601")
}
func readSpend(ctx context.Context, scope string) float64 {
	if scope == "" {
		return 0
	}
	return readCounter(ctx, monthKey(scope), "spend")
}
func addSpend(ctx context.Context, scope string, cost float64) {
	if scope == "" || cost <= 0 {
		return
	}
	// TTL ~2 months: keeps the current month and the previous one.
	bump(ctx, monthKey(scope), "spend", cost, time.Now().AddDate(0, 2, 0))
}

// addCreditSpend debits the credit consumed when the request was paid for out of it.
//
// It reuses the limits table instead of creating another one: the atomic counter with
// TTL is already exercised for RATE# and SPEND#, and credit is the same conceptual
// object — a monotonic spend counter per scope.
//
// Single-org model: the key matches creditState's read key ("CREDIT#"+provider) —
// no org segment.
func addCreditSpend(ctx context.Context, provider string, c *Config, dec routing.Decision, cost float64) {
	if dec.PaidFrom != routing.PaidFromCredit || cost <= 0 || provider == "" {
		return
	}
	// TTL aligned with the credit's expiry (+30d of slack for reconciliation).
	exp := time.Now().AddDate(0, 2, 0)
	if d, ok := c.Credits[provider]; ok && d.ExpiresAt != "" {
		if t, err := time.Parse("2006-01-02", d.ExpiresAt); err == nil {
			exp = t.UTC().AddDate(0, 0, 30)
		}
	}
	bump(ctx, "CREDIT#"+provider, "spend", cost, exp)
}
