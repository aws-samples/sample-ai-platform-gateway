// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package help

import "testing"

// sampleBundle mimics translation in progress on purpose: en is missing `usage`,
// es has nothing at all. That is the state the fallback chain has to survive.
func sampleBundle() Bundle {
	return Bundle{
		ContractVersion: "2",
		Chain:           []string{"en", "pt", "es"},
		Cat: map[string]Catalog{
			"en": {
				FAQ: map[string]Item{
					"overview": {Key: "overview", Body: "english overview", Version: 1},
				},
				Internal: map[string]Item{
					"routing": {Key: "routing", Title: "Routing", Body: "english routing", Version: 1},
				},
			},
			"pt": {
				FAQ: map[string]Item{
					"overview": {Key: "overview", Body: "visão geral", Version: 1},
					"usage":    {Key: "usage", Body: "custo", Version: 1},
				},
				Internal: map[string]Item{
					"routing": {Key: "routing", Title: "Roteamento", Body: "roteamento", Version: 1},
					"cache":   {Key: "cache", Title: "Cache", Body: "cache pt", Version: 1},
				},
			},
			"es": {FAQ: map[string]Item{}, Internal: map[string]Item{}},
		},
	}
}

func TestNormalizeLang(t *testing.T) {
	cases := map[string]string{
		"en": "en", "pt": "pt", "es": "es",
		"pt-BR": "pt", "pt_BR": "pt", "es-419": "es", "en-US": "en",
		"PT": "pt", "Es": "es",
		// anything unknown or malformed must degrade to the default, never error:
		// a bad user preference cannot deny content.
		"": "en", "fr": "en", "zz-ZZ": "en", "../../etc/passwd": "en", "pt;drop": "en",
	}
	for in, want := range cases {
		if got := NormalizeLang(in); got != want {
			t.Fatalf("NormalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
}

// The requested language wins when it has the content.
func TestFAQInExactMatch(t *testing.T) {
	b := sampleBundle()
	it, served, ok := FAQIn(b, "pt", "overview")
	if !ok || served != "pt" || it.Body != "visão geral" {
		t.Fatalf("expected the pt body, got served=%q body=%q ok=%v", served, it.Body, ok)
	}
}

// Missing translation falls through the declared chain, and served_lang reports
// which language actually answered — otherwise a gap is invisible to the client.
func TestFAQInFallsBackAndReportsLang(t *testing.T) {
	b := sampleBundle()
	it, served, ok := FAQIn(b, "es", "overview")
	if !ok {
		t.Fatal("es should fall back, not fail")
	}
	if served != "en" {
		t.Fatalf("first chain entry is en, got %q", served)
	}
	if it.Body != "english overview" {
		t.Fatalf("wrong body: %q", it.Body)
	}
	// en lacks `usage`, so the chain must continue into pt.
	it, served, ok = FAQIn(b, "es", "usage")
	if !ok || served != "pt" || it.Body != "custo" {
		t.Fatalf("expected fallback to pt, got served=%q ok=%v", served, ok)
	}
}

// A tab absent from every language is "empty", not an error.
func TestFAQInAbsentEverywhere(t *testing.T) {
	if _, _, ok := FAQIn(sampleBundle(), "en", "billing"); ok {
		t.Fatal("billing exists in no language and must not resolve")
	}
}

// An item present but with an empty body must not resolve — otherwise the panel
// renders a blank help drawer instead of falling through to a language that has text.
func TestFAQInSkipsEmptyBody(t *testing.T) {
	b := sampleBundle()
	b.Cat["es"].FAQ["overview"] = Item{Key: "overview", Body: "   "}
	_, served, ok := FAQIn(b, "es", "overview")
	if !ok || served == "es" {
		t.Fatalf("blank body should be skipped, got served=%q ok=%v", served, ok)
	}
}

// has_faq must be true when ANY language in the chain can serve it: the help
// button has to appear whenever there is servable content.
func TestPublicListInCountsFallback(t *testing.T) {
	l := PublicListIn(sampleBundle(), "es")
	if len(l) != 13 {
		t.Fatalf("expected 13 tabs, got %d", len(l))
	}
	flag := map[string]bool{}
	for _, f := range l {
		flag[f.Key] = f.HasFAQ
	}
	if !flag["overview"] || !flag["usage"] {
		t.Fatal("tabs servable via fallback must report has_faq=true")
	}
	if flag["billing"] {
		t.Fatal("a tab with no content anywhere must report has_faq=false")
	}
}

// The deep-dive list must be ordered by InternalKeys, not by map iteration, or it
// reshuffles on screen between two identical requests.
func TestInternalListInIsOrderedAndFallsBack(t *testing.T) {
	refs := InternalListIn(sampleBundle(), "es")
	if len(refs) != 2 {
		t.Fatalf("expected 2 topics, got %d (%+v)", len(refs), refs)
	}
	if refs[0].Key != "routing" || refs[1].Key != "cache" {
		t.Fatalf("order should follow InternalKeys, got %q then %q", refs[0].Key, refs[1].Key)
	}
	// routing exists in en (first in the chain), cache only in pt.
	if refs[0].Title != "Routing" || refs[1].Title != "Cache" {
		t.Fatalf("titles should come from the resolved language: %+v", refs)
	}
}

func TestInternalIn(t *testing.T) {
	b := sampleBundle()
	if it, served, ok := InternalIn(b, "pt", "cache"); !ok || served != "pt" || it.Body != "cache pt" {
		t.Fatalf("cache should resolve in pt, got served=%q ok=%v", served, ok)
	}
	if _, _, ok := InternalIn(b, "en", "sli_slo"); ok {
		t.Fatal("sli_slo exists in no language here")
	}
}

// An empty chain must not panic and must not silently serve nothing.
func TestResolveWithEmptyChain(t *testing.T) {
	b := Bundle{Chain: nil, Cat: map[string]Catalog{
		"pt": {FAQ: map[string]Item{"overview": {Body: "x"}}},
	}}
	if _, served, ok := FAQIn(b, "pt", "overview"); !ok || served != "pt" {
		t.Fatalf("requested language alone should still resolve, got served=%q ok=%v", served, ok)
	}
}
