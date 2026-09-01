// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package help

import "testing"

func sampleCatalog() Catalog {
	return Catalog{
		ContractVersion: "1",
		FAQ: map[string]Item{
			"overview": {Key: "overview", Body: "hi", Version: 1},
			"usage":    {Key: "usage", Body: "cost", Version: 1},
			// the remaining tabs have no FAQ registered
		},
		Internal: map[string]Item{
			"savings_calc": {Key: "savings_calc", Title: "Savings", Body: "secret", Version: 1},
		},
	}
}

func TestValidTab(t *testing.T) {
	if len(TabKeys) != 13 {
		t.Fatalf("expected 13 tabs, got %d", len(TabKeys))
	}
	for _, k := range TabKeys {
		if !ValidTab(k) {
			t.Fatalf("%q should be valid", k)
		}
	}
	if ValidTab("nonexistent") || ValidTab("") {
		t.Fatal("an unknown tab was accepted")
	}
}

// Property 2/3: deep-dive gating by role.
func TestCanSeeInternal(t *testing.T) {
	if !CanSeeInternal("platform_admin") {
		t.Fatal("platform_admin should see the internal content")
	}
	for _, r := range []string{"owner", "admin", "billing", "dev", "", "PLATFORM_ADMIN"} {
		if CanSeeInternal(r) {
			t.Fatalf("role %q should not see the internal content", r)
		}
	}
}

// Property 4/5: PublicList covers 13 tabs with the right flag.
func TestPublicList(t *testing.T) {
	l := PublicList(sampleCatalog())
	if len(l) != 13 {
		t.Fatalf("expected 13, got %d", len(l))
	}
	flag := map[string]bool{}
	for _, f := range l {
		flag[f.Key] = f.HasFAQ
	}
	if !flag["overview"] || !flag["usage"] {
		t.Fatal("tabs with a FAQ should report has_faq=true")
	}
	if flag["logs"] {
		t.Fatal("a tab with no FAQ should report has_faq=false")
	}
}

// Property 5: a valid tab with no FAQ => lookup ok=false (the shell returns empty,
// not an error).
func TestFAQLookup(t *testing.T) {
	c := sampleCatalog()
	if it, ok := FAQ(c, "overview"); !ok || it.Body == "" {
		t.Fatal("overview should have a FAQ")
	}
	if _, ok := FAQ(c, "logs"); ok {
		t.Fatal("logs has no FAQ registered")
	}
}

// Property 1/2: internal content is only reachable through an explicit lookup; the
// list carries key+title.
func TestInternal(t *testing.T) {
	c := sampleCatalog()
	if it, ok := Internal(c, "savings_calc"); !ok || it.Body != "secret" {
		t.Fatal("savings_calc should exist")
	}
	if _, ok := Internal(c, "nonexistent"); ok {
		t.Fatal("a nonexistent topic should not exist")
	}
	refs := InternalList(c)
	if len(refs) != 1 || refs[0].Key != "savings_calc" || refs[0].Title != "Savings" {
		t.Fatalf("unexpected topic list: %+v", refs)
	}
}
