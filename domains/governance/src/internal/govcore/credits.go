// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package govcore

import "time"

// Provider credit (PURE). Mirrors the config-api's balance/expiry arithmetic and
// the logic in Core/internal/routing/credit.go — but here Governance is the one
// that VALIDATES THE WRITE (registration/correction declared by the customer).
//
// Credit is a BALANCE, not a discount: the price per token does not change, only
// which pocket it comes out of. The consumption we show is a LOWER BOUND (we only
// see what goes through the gateway), which is why the customer's manual
// correction replaces the declared amount as the base.

const dateLayout = "2006-01-02"

// Remaining is the estimated balance: base − consumed, floored at zero.
// The manual correction (corrected > 0), when present, replaces the declared
// amount as the base — the customer just looked at the invoice and knows more
// than we do.
func Remaining(amountUSD, correctedUSD, consumedUSD float64) float64 {
	base := amountUSD
	if correctedUSD > 0 {
		base = correctedUSD
	}
	if r := base - consumedUSD; r > 0 {
		return r
	}
	return 0
}

// Expired tells whether the credit is past its validity. A credit is valid until
// the END of the `expires_at` day (hence +1 day). An empty date = no expiry; an
// invalid date is treated as not expired (parsing is the write validation's
// responsibility).
func Expired(expiresAt string, now time.Time) bool {
	if expiresAt == "" {
		return false
	}
	t, err := time.Parse(dateLayout, expiresAt)
	if err != nil {
		return false
	}
	return now.After(t.AddDate(0, 0, 1))
}

// Active: the credit only counts when it has not expired AND there is balance left.
func Active(expired bool, remaining float64) bool {
	return !expired && remaining > 0
}

// ValidDate validates the `expires_at` format at WRITE time. Empty is valid (it
// removes the expiry). Only YYYY-MM-DD is accepted.
func ValidDate(expiresAt string) bool {
	if expiresAt == "" {
		return true
	}
	_, err := time.Parse(dateLayout, expiresAt)
	return err == nil
}

// NonNegative is the write rule for amount_usd and corrected_remaining_usd:
// neither of them may be negative.
func NonNegative(v float64) bool { return v >= 0 }
