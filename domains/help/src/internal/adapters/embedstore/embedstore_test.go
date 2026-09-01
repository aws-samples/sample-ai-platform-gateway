// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package embedstore

import (
	"context"
	"strings"
	"testing"

	"github.com/aiplat/help/internal/help"
)

// This is the test that catches a manifest path typo or a file that was moved
// without updating the manifest. The pure-domain tests use fixtures, so only this
// one exercises the REAL embedded tree.
func TestLoadRealContent(t *testing.T) {
	b, err := New().Load(context.Background())
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if b.ContractVersion == "" {
		t.Fatal("contract_version missing from the manifest")
	}
	if len(b.Chain) == 0 {
		t.Fatal("empty fallback chain — manifest.languages is required")
	}
	if b.Chain[0] != help.DefaultLang {
		t.Fatalf("English is the source language, so it must lead the chain; got %q", b.Chain[0])
	}
}

// Every one of the 13 tabs must resolve for every supported language. Because the
// chain falls back, this passes as soon as ONE language has the file — which is
// exactly the guarantee we want: the user never sees an empty help drawer.
func TestEveryTabResolvesInEveryLanguage(t *testing.T) {
	b, err := New().Load(context.Background())
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	for _, lang := range help.SupportedLangs {
		for _, tab := range help.TabKeys {
			it, served, ok := help.FAQIn(b, lang, tab)
			if !ok {
				t.Errorf("tab %q does not resolve in any language (requested %q)", tab, lang)
				continue
			}
			if strings.TrimSpace(it.Body) == "" {
				t.Errorf("tab %q resolved to an empty body (lang %q)", tab, served)
			}
		}
	}
}

// Same for the deep-dive topics declared in InternalKeys: a key listed there with
// no file anywhere would render a topic that opens blank.
func TestEveryInternalTopicResolves(t *testing.T) {
	b, err := New().Load(context.Background())
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	for _, lang := range help.SupportedLangs {
		for _, key := range help.InternalKeys {
			if _, _, ok := help.InternalIn(b, lang, key); !ok {
				t.Errorf("deep-dive topic %q does not resolve in any language (requested %q)", key, lang)
			}
		}
	}
}

// Reports which languages are still incomplete. Informational: it never fails, so
// shipping one language at a time stays green — but the gap is visible in test
// output instead of only in production logs.
func TestTranslationCoverage(t *testing.T) {
	b, err := New().Load(context.Background())
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	total := len(help.TabKeys) + len(help.InternalKeys)
	for _, lang := range b.Chain {
		cat := b.Catalog(lang)
		have := 0
		for _, k := range help.TabKeys {
			if it, ok := cat.FAQ[k]; ok && strings.TrimSpace(it.Body) != "" {
				have++
			}
		}
		for _, k := range help.InternalKeys {
			if it, ok := cat.Internal[k]; ok && strings.TrimSpace(it.Body) != "" {
				have++
			}
		}
		t.Logf("coverage %s: %d/%d", lang, have, total)
	}
}

// The deep-dive title must be present in the resolved language. A missing per-language
// title in the manifest shows a heading in the wrong language above the prose.
func TestInternalTitlesArePresent(t *testing.T) {
	b, err := New().Load(context.Background())
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	for _, lang := range help.SupportedLangs {
		for _, ref := range help.InternalListIn(b, lang) {
			if strings.TrimSpace(ref.Title) == "" {
				t.Errorf("topic %q has no title for lang %q (check manifest.internal.*.title)", ref.Key, lang)
			}
		}
	}
}
