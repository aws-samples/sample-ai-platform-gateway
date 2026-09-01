// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Aggregation of the swap dimension (model-identity-bundles, task 9).
package telemetry

import "testing"

func aggByKey(list []Agg, key string) (Agg, bool) {
	for _, a := range list {
		if a.Key == key {
			return a, true
		}
	}
	return Agg{}, false
}

// Arbitrage money must land in the VERIFIED column. If it fell into the
// counterfactual one, the ledger would contradict itself: savings_class says
// verified while the amount sits in the column that needs an assumed baseline —
// and gain-share leans on the verified column.
func TestBySwapClass_ArbitrageIsVerified(t *testing.T) {
	recs := []Record{
		{Model: "gpt-azure", RequestedModel: "gpt-openai", SwapClass: SwapSameModel,
			Reason: "provider_arbitrage", Cost: 0.0077, Saved: 0.0077,
			SavedVerified: 0.0077, TS: "2026-08-14T10:00:00Z"},
		{Model: "haiku", RequestedModel: "sonnet", SwapClass: SwapDowngrade,
			Reason: "auto_cheapest", Cost: 0.001, Saved: 0.02,
			SavedCounterfactual: 0.02, TS: "2026-08-14T11:00:00Z"},
		{Model: "sonnet", RequestedModel: "sonnet", SwapClass: SwapNone,
			Cost: 0.05, TS: "2026-08-14T12:00:00Z"},
	}
	total := Totals(recs)
	if total.SavedVer != 0.0077 {
		t.Errorf("verified = %v, want 0.0077 (arbitrage only)", total.SavedVer)
	}
	if total.SavedCf != 0.02 {
		t.Errorf("counterfactual = %v, want 0.02 (the model change only)", total.SavedCf)
	}

	by := BySwapClass(recs)
	if len(by) != 3 {
		t.Fatalf("want 3 classes, got %d: %+v", len(by), by)
	}
	// Served-as-requested must be an explicit line, not a missing one: "how much of
	// my traffic ran on the model I asked for" is the baseline question.
	none, ok := aggByKey(by, "none")
	if !ok || none.Requests != 1 {
		t.Errorf("no-swap bucket = %+v, want 1 request under key none", none)
	}
	same, _ := aggByKey(by, SwapSameModel)
	if same.SavedVer != 0.0077 || same.SavedCf != 0 {
		t.Errorf("same_model bucket ver/cf = %v/%v, want 0.0077/0", same.SavedVer, same.SavedCf)
	}
	down, _ := aggByKey(by, SwapDowngrade)
	if down.SavedCf != 0.02 || down.SavedVer != 0 {
		t.Errorf("downgrade bucket ver/cf = %v/%v, want 0/0.02", down.SavedVer, down.SavedCf)
	}
}

// A record written before the split existed carries only Saved. Classifying it by
// reason keeps history reconcilable, and arbitrage has to be on the verified side
// of that fallback too — otherwise the same event would be filed differently
// depending on when it was written.
func TestBySwapClass_LegacyRecordClassifiedByReason(t *testing.T) {
	cases := []struct {
		reason     string
		wantVerify bool
	}{
		{"cache", true},
		{"provider_prompt_cache", true},
		{"provider_arbitrage", true},
		{"auto_cheapest", false},
		{"fallback", false},
		{"semantic_cache", false},
	}
	for _, c := range cases {
		total := Totals([]Record{{Reason: c.reason, Cost: 0.01, Saved: 0.03, TS: "2026-08-14T10:00:00Z"}})
		gotVerified := total.SavedVer == 0.03
		if gotVerified != c.wantVerify {
			t.Errorf("reason %q: verified=%v cf=%v, want verified=%v",
				c.reason, total.SavedVer, total.SavedCf, c.wantVerify)
		}
		// The invariant that keeps the ledger reconcilable, whatever the class.
		if total.SavedVer+total.SavedCf != total.SavedUSD {
			t.Errorf("reason %q: ver+cf = %v, want == saved %v", c.reason, total.SavedVer+total.SavedCf, total.SavedUSD)
		}
	}
}

// An old record has no swap_class at all. It must not be invented: absence lands
// in `none` (served as requested is the only thing we can say without guessing).
func TestBySwapClass_OldRecordsWithoutTheField(t *testing.T) {
	by := BySwapClass([]Record{
		{Model: "m1", Cost: 0.01, TS: "2026-08-01T10:00:00Z"},
		{Model: "m2", RequestedModel: "m1", Cost: 0.01, TS: "2026-08-01T11:00:00Z"},
	})
	if len(by) != 1 {
		t.Fatalf("want a single bucket, got %+v", by)
	}
	if by[0].Key != "none" || by[0].Requests != 2 {
		t.Errorf("bucket = %+v, want key none with 2 requests", by[0])
	}
}
