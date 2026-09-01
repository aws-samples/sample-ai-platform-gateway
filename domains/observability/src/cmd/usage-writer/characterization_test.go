// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// CHARACTERIZATION test of the usage-writer (hexagonal-refactor, task 15.4).
//
// Captures the CURRENT behavior of the Cost_Store item assembly (buildItem) and, in
// particular, the basis of IDEMPOTENCY: the sort key sk embeds ts + request_id, so
// reprocessing the SAME message produces the SAME sk — and the handler's
// ConditionExpression attribute_not_exists(sk) deduplicates without double counting.
// buildItem is a pure function (no IO): the same input produces the same item.
//
// The usage-writer stays a shell (D8/task 17.3): buildItem does NOT get a port; it is
// only verified as a pure function. Runs offline: package main, without touching DynamoDB.
package main

import (
	"strconv"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func sv(av ddbtypes.AttributeValue) string {
	if s, ok := av.(*ddbtypes.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}
func nv(av ddbtypes.AttributeValue) string {
	if n, ok := av.(*ddbtypes.AttributeValueMemberN); ok {
		return n.Value
	}
	return ""
}
func bv(av ddbtypes.AttributeValue) bool {
	if b, ok := av.(*ddbtypes.AttributeValueMemberBOOL); ok {
		return b.Value
	}
	return false
}

func sampleUsage() usage {
	return usage{
		RequestID: "req-123", Team: "sre", AppTag: "api", Feature: "code",
		Provider: "bedrock", Upstream: "bedrock", Model: "claude",
		TokensIn: 100, TokensOut: 50, Cost: 1.5, Saved: 0.5, SavingsReason: "cache",
		SavedVerified: 0.5, SavedCounterfactual: 0, SavingsClass: "verified",
		LatencyMs: 200, CacheHit: true, Status: "success",
		PaidFrom: "cash", CreditUSD: 0, CashUSD: 1.5, PriceSource: "contract",
		SLIEligible: true, Ts: "2026-01-02T10:00:00Z",
	}
}

// TestChar_IdempotentKey: the sk is a function of ts+request_id only — that is what
// makes reprocessing the SAME message collide on the existence condition instead of
// duplicating.
func TestChar_IdempotentKey(t *testing.T) {
	u := sampleUsage()
	item := buildItem(u)
	if got := sv(item["sk"]); got != "TS#2026-01-02T10:00:00Z#req-123" {
		t.Errorf("sk = %q, want TS#2026-01-02T10:00:00Z#req-123", got)
	}
	if got := sv(item["pk"]); got != "USAGE" {
		t.Errorf("pk = %q, want USAGE", got)
	}
	if got := sv(item["gsi1pk"]); got != "APP#api" {
		t.Errorf("gsi1pk = %q, want APP#api", got)
	}
}

// TestChar_Deterministic: buildItem is pure — two assemblies of the same message
// produce identical items key by key (which guarantees reprocessing is harmless).
func TestChar_Deterministic(t *testing.T) {
	u := sampleUsage()
	a, b := buildItem(u), buildItem(u)
	if len(a) != len(b) {
		t.Fatalf("different sizes: %d vs %d", len(a), len(b))
	}
	for k := range a {
		if sv(a[k]) != sv(b[k]) || nv(a[k]) != nv(b[k]) || bv(a[k]) != bv(b[k]) {
			t.Errorf("field %q differs between two assemblies", k)
		}
	}
}

// TestChar_Defaults: records without app/feature/team/status get the compatibility
// defaults — behavior the downstream aggregation assumes.
func TestChar_Defaults(t *testing.T) {
	u := usage{RequestID: "r1", Ts: "2026-01-01T00:00:00Z"}
	item := buildItem(u)
	if sv(item["app_tag"]) != "none" || sv(item["feature"]) != "none" {
		t.Errorf("app/feature default = %q/%q, want none/none", sv(item["app_tag"]), sv(item["feature"]))
	}
	if sv(item["team"]) != "default" {
		t.Errorf("team default = %q, want default", sv(item["team"]))
	}
	if sv(item["status"]) != "success" {
		t.Errorf("status default = %q, want success", sv(item["status"]))
	}
}

// TestChar_Partitions: the per-pocket partition and the savings by class are persisted
// as numeric fields (the ledger reconciles credit+cash==cost and
// verified+counterfactual==saved downstream).
func TestChar_Partitions(t *testing.T) {
	u := sampleUsage()
	item := buildItem(u)
	if nv(item["cash_usd"]) != "1.5" || nv(item["credit_usd"]) != "0" {
		t.Errorf("cash/credit = %q/%q, want 1.5/0", nv(item["cash_usd"]), nv(item["credit_usd"]))
	}
	if nv(item["saved_verified_usd"]) != "0.5" || nv(item["saved_counterfactual_usd"]) != "0" {
		t.Errorf("saved ver/cf = %q/%q, want 0.5/0", nv(item["saved_verified_usd"]), nv(item["saved_counterfactual_usd"]))
	}
	if sv(item["price_source"]) != "contract" {
		t.Errorf("price_source = %q, want contract", sv(item["price_source"]))
	}
	if !bv(item["sli_eligible"]) {
		t.Errorf("sli_eligible should be true")
	}
}

// TestChar_TTLFixo: expires_at is computed from the record's OWN ts (not the
// instant of processing) — same rule as the audit trail. A message that sat in the
// DLQ and got reprocessed days later must not gain extra retention, otherwise a
// slow/retried record would silently outlive its faster siblings.
func TestChar_TTLFixo(t *testing.T) {
	u := sampleUsage() // Ts: "2026-01-02T10:00:00Z"
	item := buildItem(u)

	base, err := time.Parse(time.RFC3339, u.Ts)
	if err != nil {
		t.Fatalf("bad fixture ts: %v", err)
	}
	want := base.Add(hotRetention).Unix()
	got, err := strconv.ParseInt(nv(item["expires_at"]), 10, 64)
	if err != nil {
		t.Fatalf("expires_at is not a number: %v", err)
	}
	if got != want {
		t.Errorf("expires_at = %d, want %d (ts + hotRetention)", got, want)
	}
}

// TestChar_TTLFallsBackToNowOnBadTs: a malformed ts must not leave expires_at unset
// (TTL silently not applying is worse than a slightly wrong expiry).
func TestChar_TTLFallsBackToNowOnBadTs(t *testing.T) {
	before := time.Now().UTC()
	u := sampleUsage()
	u.Ts = "not-a-timestamp"
	item := buildItem(u)
	after := time.Now().UTC()

	got, err := strconv.ParseInt(nv(item["expires_at"]), 10, 64)
	if err != nil {
		t.Fatalf("expires_at is not a number: %v", err)
	}
	lo, hi := before.Add(hotRetention).Unix(), after.Add(hotRetention).Unix()
	if got < lo || got > hi {
		t.Errorf("expires_at = %d, want within [%d, %d] (now + hotRetention)", got, lo, hi)
	}
}
