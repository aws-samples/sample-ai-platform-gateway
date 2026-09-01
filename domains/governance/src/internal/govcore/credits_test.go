// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package govcore

import (
	"testing"
	"testing/quick"
	"time"
)

func TestRemaining(t *testing.T) {
	cases := []struct {
		amount, corrected, consumed, want float64
	}{
		{100, 0, 30, 70},  // no correction: base=amount
		{100, 60, 30, 30}, // the correction replaces the base
		{100, 0, 200, 0},  // consumed beyond the balance → floored at zero
		{100, 60, 60, 0},  // correction exhausted
		{0, 0, 0, 0},      // all zeros
		{100, 0, 0, 100},  // nothing consumed
	}
	for _, c := range cases {
		if got := Remaining(c.amount, c.corrected, c.consumed); got != c.want {
			t.Errorf("Remaining(%v,%v,%v)=%v, want %v", c.amount, c.corrected, c.consumed, got, c.want)
		}
	}
}

// Property: Remaining is never negative.
func TestRemainingNeverNegative(t *testing.T) {
	prop := func(amount, corrected, consumed float64) bool {
		return Remaining(amount, corrected, consumed) >= 0
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 5000}); err != nil {
		t.Error(err)
	}
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		expires string
		want    bool
	}{
		{"", false},           // no expiry
		{"2026-06-14", true},  // yesterday → expired (valid until end of day +1)
		{"2026-06-15", false}, // today → still valid (end of day)
		{"2026-06-16", false}, // future
		{"lixo", false},       // invalid → treated as not expired
	}
	for _, c := range cases {
		if got := Expired(c.expires, now); got != c.want {
			t.Errorf("Expired(%q)=%v, want %v", c.expires, got, c.want)
		}
	}
}

func TestActive(t *testing.T) {
	if !Active(false, 10) {
		t.Error("not expired with balance must be active")
	}
	if Active(true, 10) {
		t.Error("expired is never active")
	}
	if Active(false, 0) {
		t.Error("no balance is never active")
	}
}

func TestValidDate(t *testing.T) {
	cases := map[string]bool{"": true, "2026-01-31": true, "2026-13-01": false, "31-01-2026": false, "abc": false}
	for s, want := range cases {
		if got := ValidDate(s); got != want {
			t.Errorf("ValidDate(%q)=%v, want %v", s, got, want)
		}
	}
}

func TestNonNegative(t *testing.T) {
	if !NonNegative(0) || !NonNegative(1.5) || NonNegative(-0.01) {
		t.Error("NonNegative is wrong")
	}
}
