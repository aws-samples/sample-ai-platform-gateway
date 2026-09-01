// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/quick"

	"github.com/aiplat/core/internal/ports"
)

func msg(role, text string) ports.Message { return ports.Message{Role: role, Text: text} }

func fptr(f float64) *float64 { return &f }
func iptr(i int) *int         { return &i }

// P1: Canonicalize is idempotent.
func TestCanonicalizeIdempotent(t *testing.T) {
	f := func(s string) bool { return Canonicalize(Canonicalize(s)) == Canonicalize(s) }
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// Concrete cases from the HR scenario: trivial variations collide on the canonical key.
func TestCanonicalizeColapsaVariacoes(t *testing.T) {
	base := Canonicalize("quantos dias de férias eu tenho?")
	variantes := []string{
		"Quantos dias de férias eu tenho?",
		"quantos dias de ferias eu tenho",
		"quantos  dias   de férias eu tenho!!!",
		"  quantos dias de férias eu tenho.  ",
	}
	for _, v := range variantes {
		if got := Canonicalize(v); got != base {
			t.Errorf("Canonicalize(%q)=%q, want %q", v, got, base)
		}
	}
	// Negation and numbers must NOT collapse (different meaning).
	if Canonicalize("posso vender 10 dias") == Canonicalize("posso vender 20 dias") {
		t.Error("different numbers must not collide")
	}
}

// P4: tools take part in the identity — a request with tools ≠ without tools.
func TestKeyToolsChangesIdentity(t *testing.T) {
	in := KeyInput{Org: "o", Model: "m", Messages: []ports.Message{msg("user", "oi")}}
	semTools := CacheKey(in, KeyExact)
	in.Tools = []ports.ToolDef{{Name: "get_weather", Parameters: map[string]interface{}{"type": "object"}}}
	comTools := CacheKey(in, KeyExact)
	if semTools == comTools {
		t.Error("a request with tools must have a different key than one without (R2 defect)")
	}
}

// temperature and max_tokens take part in the identity; nil ≠ 0.
func TestKeyTemperatureAndMaxTokens(t *testing.T) {
	base := KeyInput{Org: "o", Model: "m", Messages: []ports.Message{msg("user", "oi")}}
	k0 := CacheKey(base, KeyExact)

	withT := base
	withT.Temperature = fptr(0.7)
	if CacheKey(withT, KeyExact) == k0 {
		t.Error("temperature must change the key")
	}
	// temperature=0 (deterministic, explicitly requested) ≠ absent.
	withT0 := base
	withT0.Temperature = fptr(0)
	if CacheKey(withT0, KeyExact) == k0 {
		t.Error("an explicit temperature=0 ≠ absent")
	}
	withM := base
	withM.MaxTokens = iptr(256)
	if CacheKey(withM, KeyExact) == k0 {
		t.Error("max_tokens must change the key")
	}
}

// P3: the key never collides across orgs, for any input.
func TestKeyNeverCollidesAcrossOrgs(t *testing.T) {
	f := func(orgA, orgB, text string, canonical bool) bool {
		if orgA == orgB {
			return true
		}
		mode := KeyExact
		if canonical {
			mode = KeyCanonical
		}
		mk := func(org string) string {
			return CacheKey(KeyInput{Org: org, Model: "m", Messages: []ports.Message{msg("user", text)}}, mode)
		}
		return mk(orgA) != mk(orgB)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// Canonical mode makes trivial variations collide; exact mode does NOT.
func TestKeyCanonicalVsExact(t *testing.T) {
	a := KeyInput{Org: "o", Model: "m", Messages: []ports.Message{msg("user", "Férias?")}}
	b := KeyInput{Org: "o", Model: "m", Messages: []ports.Message{msg("user", "ferias")}}
	if CacheKey(a, KeyExact) == CacheKey(b, KeyExact) {
		t.Error("exact mode must not collapse variations")
	}
	if CacheKey(a, KeyCanonical) != CacheKey(b, KeyCanonical) {
		t.Error("canonical mode must collapse case/accents/punctuation")
	}
}

// The key is deterministic (same input → same key).
func TestKeyDeterministic(t *testing.T) {
	in := KeyInput{Org: "o", Model: "m",
		Messages:  []ports.Message{msg("system", "seja breve"), msg("user", "oi")},
		Tools:     []ports.ToolDef{{Name: "f", Parameters: map[string]interface{}{"a": 1, "b": 2}}},
		MaxTokens: iptr(100), Temperature: fptr(0.2)}
	if CacheKey(in, KeyCanonical) != CacheKey(in, KeyCanonical) {
		t.Error("the key is not deterministic")
	}
}

// NormalizeKeyMode: unknown/empty → exact (the safe default).
func TestNormalizeKeyMode(t *testing.T) {
	for _, s := range []string{"", "exact", "banana", "EXACT"} {
		if NormalizeKeyMode(s) != KeyExact {
			t.Errorf("NormalizeKeyMode(%q) should be exact", s)
		}
	}
	if NormalizeKeyMode("canonical") != KeyCanonical {
		t.Error("canonical should be recognized")
	}
}

// Canonical equates variations of case/accent/space/TRAILING punctuation. INTERNAL
// punctuation (a comma in the middle) is preserved on purpose — removing it could
// change meaning. Hence "OLÁ MUNDO!!!" ≡ "ola mundo", but "olá, mundo" does NOT.
func TestCanonicalFold(t *testing.T) {
	x := CacheKey(KeyInput{Org: "o", Model: "m", Messages: []ports.Message{msg("user", "  OLÁ   MUNDO!!! ")}}, KeyCanonical)
	y := CacheKey(KeyInput{Org: "o", Model: "m", Messages: []ports.Message{msg("user", "ola mundo")}}, KeyCanonical)
	if x != y {
		t.Error("canonical should equate 'OLÁ MUNDO!!!' and 'ola mundo'")
	}
	// internal comma preserved → different key.
	z := CacheKey(KeyInput{Org: "o", Model: "m", Messages: []ports.Message{msg("user", "olá, mundo")}}, KeyCanonical)
	if z == y {
		t.Error("internal punctuation should not be removed")
	}
	_ = strings.TrimSpace
	_ = json.Marshal
}
