// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package auditcore

import (
	"encoding/json"
	"strings"
	"testing"
)

func obj(s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return m
}

func find(chs []Change, path string) (Change, bool) {
	for _, c := range chs {
		if c.Path == path {
			return c, true
		}
	}
	return Change{}, false
}

func TestDiff_CampoAlterado(t *testing.T) {
	chs := Diff(
		obj(`{"budget":{"limit_usd":100,"action":"alert"}}`),
		obj(`{"budget":{"limit_usd":50,"action":"degrade"}}`),
	)
	if len(chs) != 2 {
		t.Fatalf("changes = %d, expected 2: %+v", len(chs), chs)
	}
	c, ok := find(chs, "budget.limit_usd")
	if !ok {
		t.Fatal("budget.limit_usd is missing")
	}
	if c.Before != float64(100) || c.After != float64(50) {
		t.Errorf("before/after = %v/%v, expected 100/50", c.Before, c.After)
	}
}

// An unchanged field is not a change — otherwise every config PUT would produce a diff
// with the whole config, and the trail would stop answering "what changed".
func TestDiff_CampoInalteradoNaoEntra(t *testing.T) {
	chs := Diff(
		obj(`{"a":1,"b":2}`),
		obj(`{"a":1,"b":3}`),
	)
	if len(chs) != 1 || chs[0].Path != "b" {
		t.Fatalf("expected only 'b': %+v", chs)
	}
}

func TestDiff_CriacaoNaoTemBefore(t *testing.T) {
	chs := Diff(obj(`{}`), obj(`{"guardrails":{"block_secrets":true}}`))
	c, ok := find(chs, "guardrails.block_secrets")
	if !ok {
		t.Fatalf("the created field is missing: %+v", chs)
	}
	if c.Before != nil {
		t.Errorf("a created field should not have a Before: %v", c.Before)
	}
	if c.After != true {
		t.Errorf("After = %v, expected true", c.After)
	}
}

func TestDiff_RemocaoNaoTemAfter(t *testing.T) {
	chs := Diff(obj(`{"rate_limits":{"requests_per_minute":60}}`), obj(`{}`))
	c, ok := find(chs, "rate_limits.requests_per_minute")
	if !ok {
		t.Fatalf("the removed field is missing: %+v", chs)
	}
	if c.After != nil {
		t.Errorf("a removed field should not have an After: %v", c.After)
	}
	if c.Before != float64(60) {
		t.Errorf("Before = %v, expected 60", c.Before)
	}
}

func TestDiff_Aninhamento(t *testing.T) {
	chs := Diff(
		obj(`{"routing":{"m1":{"provider":"bedrock","capabilities":{"tool_use":false}}}}`),
		obj(`{"routing":{"m1":{"provider":"bedrock","capabilities":{"tool_use":true}}}}`),
	)
	if len(chs) != 1 {
		t.Fatalf("expected 1 change: %+v", chs)
	}
	if chs[0].Path != "routing.m1.capabilities.tool_use" {
		t.Errorf("path = %q", chs[0].Path)
	}
}

// The order must be a function only of the set of paths. Without that the test would be
// flaky (a Go map iterates out of order) and the STORED diff would change shape between
// two identical runs — unacceptable in an immutable record.
func TestDiff_OrdemDeterministica(t *testing.T) {
	before := obj(`{"z":1,"a":1,"m":1,"b":1,"y":1}`)
	after := obj(`{"z":2,"a":2,"m":2,"b":2,"y":2}`)
	var first []string
	for i := 0; i < 20; i++ {
		chs := Diff(before, after)
		paths := make([]string, len(chs))
		for j, c := range chs {
			paths[j] = c.Path
		}
		if i == 0 {
			first = paths
			continue
		}
		if strings.Join(paths, ",") != strings.Join(first, ",") {
			t.Fatalf("order varied between runs: %v vs %v", first, paths)
		}
	}
	if strings.Join(first, ",") != "a,b,m,y,z" {
		t.Errorf("order = %v, expected alphabetical", first)
	}
}

// A number coming from JSON is a float64; 50 and 50.0 are the SAME value and must not
// show up as a change.
func TestDiff_NumeroEquivalenteNaoEMudanca(t *testing.T) {
	chs := Diff(obj(`{"x":50}`), obj(`{"x":50.0}`))
	if len(chs) != 0 {
		t.Errorf("50 vs 50.0 is not a change: %+v", chs)
	}
}

// A list is compared as a single value: an array index is an unstable path and inserting
// at the front would make "every index changed".
func TestDiff_ListaComparadaComoValorUnico(t *testing.T) {
	chs := Diff(obj(`{"allowed_models":["a","b"]}`), obj(`{"allowed_models":["x","a","b"]}`))
	if len(chs) != 1 || chs[0].Path != "allowed_models" {
		t.Fatalf("expected one change on allowed_models: %+v", chs)
	}
}

// --- Redaction: the trap case --------------------------------------------------

func TestIsSensitivePath_ValorDeCredencialXNomeDoSegredo(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// credential value: redact
		{"api_key", true},
		{"routing.openrouter.api_key", true},
		{"password", true},
		{"member.token", true},
		{"provider.private_key", true},
		{"API_KEY", true}, // case-insensitive
		// NAME of the referenced secret: do NOT redact — that is what we want to audit
		{"routing.openrouter.api_key_secret", false},
		{"api_key_secret", false},
		{"secret_name", false},
		// ordinary fields
		{"budget.limit_usd", false},
		{"guardrails.block_secrets", false},
	}
	for _, tc := range tests {
		if got := IsSensitivePath(tc.path); got != tc.want {
			t.Errorf("IsSensitivePath(%q) = %v, expected %v", tc.path, got, tc.want)
		}
	}
}

func TestRedact_PreservaOFatoDaMudanca(t *testing.T) {
	chs := Redact(Diff(
		obj(`{"provider":{"api_key":"sk-antigo-123"}}`),
		obj(`{"provider":{"api_key":"sk-novo-456"}}`),
	))
	if len(chs) != 1 {
		t.Fatalf("expected 1 change: %+v", chs)
	}
	c := chs[0]
	if !c.Redacted {
		t.Error("a sensitive change should be marked as redacted")
	}
	if c.Before != RedactedMarker || c.After != RedactedMarker {
		t.Errorf("values not redacted: %v / %v", c.Before, c.After)
	}
}

// Property 2: no sensitive value survives the serialized Diff+Redact pair.
// This is the test that stops the audit from becoming a credential repository.
func TestPropriedade_NenhumSegredoSobrevive(t *testing.T) {
	segredos := []string{"sk-super-secreto-abc", "MinhaSenh@123", "ghp_tokenzinho", "chave-privada-xyz"}
	before := obj(`{
	  "provider":{"api_key":"sk-super-secreto-abc","base_url":"https://x.dev"},
	  "user":{"password":"MinhaSenh@123","email":"a@b.com"},
	  "git":{"access_token":"ghp_tokenzinho"},
	  "tls":{"private_key":"chave-privada-xyz"}
	}`)
	after := obj(`{
	  "provider":{"api_key":"sk-outro-valor","base_url":"https://y.dev"},
	  "user":{"password":"OutraSenha!","email":"a@b.com"},
	  "git":{"access_token":"ghp_outro"},
	  "tls":{"private_key":"outra-chave"}
	}`)

	b, err := json.Marshal(Redact(Diff(before, after)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, s := range segredos {
		if strings.Contains(got, s) {
			t.Errorf("secret %q leaked into the serialized record: %s", s, got)
		}
	}
	// And non-sensitive data stays auditable (otherwise redaction would be eating
	// everything and the trail would be useless).
	if !strings.Contains(got, "base_url") {
		t.Error("a non-sensitive field should stay in the diff")
	}
}

// --- Truncation ---------------------------------------------------------------

func TestTruncate(t *testing.T) {
	mk := func(n int) []Change {
		out := make([]Change, n)
		for i := range out {
			out[i] = Change{Path: "p"}
		}
		return out
	}
	if chs, cut := Truncate(mk(5), 10); cut || len(chs) != 5 {
		t.Errorf("below the ceiling should not cut: len=%d cut=%v", len(chs), cut)
	}
	if chs, cut := Truncate(mk(10), 10); cut || len(chs) != 10 {
		t.Errorf("exactly at the ceiling should not cut: len=%d cut=%v", len(chs), cut)
	}
	if chs, cut := Truncate(mk(11), 10); !cut || len(chs) != 10 {
		t.Errorf("above the ceiling should cut: len=%d cut=%v", len(chs), cut)
	}
	if chs, cut := Truncate(mk(3), 0); cut || len(chs) != 3 {
		t.Errorf("a zero ceiling means no limit: len=%d cut=%v", len(chs), cut)
	}
}
