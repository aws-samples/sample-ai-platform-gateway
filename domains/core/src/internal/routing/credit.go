// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import "time"

// Remaining is the estimated credit balance, with a floor of zero (Req 15.4).
//
// IMPORTANT: this balance is a LOWER BOUND on real consumption. Our ledger only sees
// what goes through the gateway, and provider credit (AWS Activate, for example) is
// consumed by the whole account — S3, EC2, Lambda. The UI is required to present it
// as an estimate and to offer manual correction (Req 15.8/15.9); presenting it as
// exact would lead the customer to blow through the credit trusting our number.
func Remaining(declaredUSD, correctedUSD, consumedUSD Money) Money {
	base := declaredUSD
	if correctedUSD > 0 {
		base = correctedUSD
	}
	if r := base - consumedUSD; r > 0 {
		return r
	}
	return 0
}

// CashCost projects the gross cost into real cash outlay (Req 16.1, 16.3).
//
// Credit is a BALANCE, not a discount: the price per token stays identical (Req 15.5).
// What changes is which pocket it comes out of. Within the validity window, a model
// covered by credit has zero marginal cost in cash, and burning the credit before it
// expires is the rational decision.
//
// Returns (cash cost, source). A request that does NOT fit in the remaining balance
// is treated entirely as real money — there is no partial split, because splitting a
// single request between credit and cash would make the ledger irreconcilable.
func CashCost(gross Money, provider string, cs *CreditState, now time.Time) (Money, string) {
	if cs == nil || len(cs.ByProvider) == 0 {
		return gross, PaidFromCash
	}
	cr, ok := cs.ByProvider[provider]
	if !ok {
		return gross, PaidFromCash
	}
	// Expired credit does not count, even with balance left over (Req 15.7).
	if !cr.ExpiresAt.IsZero() && now.After(cr.ExpiresAt) {
		return gross, PaidFromCash
	}
	if cr.RemainingUSD <= 0 {
		return gross, PaidFromCash
	}
	if gross > cr.RemainingUSD {
		return gross, PaidFromCash
	}
	return 0, PaidFromCredit
}

// CreditExhausted tells whether enforcement should move to ceiling 2 — the real-money
// budget that already exists, with the alert/degrade/block action (Req 15.6, 15.7).
// Credit is not a new blocking mechanism: it is a stage before it.
func CreditExhausted(cr Credit, now time.Time) bool {
	if !cr.ExpiresAt.IsZero() && now.After(cr.ExpiresAt) {
		return true
	}
	return cr.RemainingUSD <= 0
}
