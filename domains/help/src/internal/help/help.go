// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package help is the PURE domain of Help & FAQ: it validates tab keys, separates
// the two audiences (public FAQ × internal deep-dive), decides audience by role and
// resolves the language through the fallback chain.
//
// Boundary rule (hexagonal): nothing here reaches an SDK, the network, a file or
// embed. Content arrives as DATA (Bundle), loaded by an adapter. That makes the
// domain testable without IO and the audience split verifiable by unit test.
package help

import "strings"

// TabKeys are the Console's 13 tabs — the CLOSED set of valid FAQ keys.
var TabKeys = []string{
	"overview", "usage", "roi", "logs", "play", "models",
	"limits", "guardrails", "teams", "keys", "alerts", "billing", "settings",
}

// InternalKeys are the deep-dive topics, in PRESENTATION ORDER. It exists for two
// reasons: it closes the set of acceptable keys (same role as TabKeys) and it gives
// the list a stable order — iterating the catalog map returned the topics in a
// different order on every call, which made the list jump around on screen.
var InternalKeys = []string{
	"routing", "cache", "savings_calc", "config_scope",
	"sli_slo", "failures", "limits_budget", "guardrails",
}

// RolePlatformAdmin is the only role that sees the internal deep-dive.
const RolePlatformAdmin = "platform_admin"

// Item is a unit of content already read from the source.
type Item struct {
	Key     string `json:"key"`
	Title   string `json:"title,omitempty"`
	Body    string `json:"body"`
	Version int    `json:"version"`
}

// Catalog is all the content loaded for ONE language (public + internal) plus the
// contract version.
type Catalog struct {
	FAQ             map[string]Item
	Internal        map[string]Item
	ContractVersion string
}

// ---------------------------------------------------------------------------
// Multi-language
//
// English is the source language (see aiplat-language-policy.md), so it is the
// first link of the fallback chain. Bundle holds one Catalog per language; the
// resolution belongs to the DOMAIN (not the adapter) so it is testable without IO
// — and because "which language the user actually got" is a decision, not a
// detail of reading a file.
// ---------------------------------------------------------------------------

// DefaultLang is the language served when nothing else resolves.
const DefaultLang = "en"

// SupportedLangs are the languages the API accepts in ?lang=. Closed on purpose:
// the language comes from the client, and an open set would turn into a path-read
// vector, since the value is used to build a file path.
var SupportedLangs = []string{"en", "pt", "es"}

// NormalizeLang maps whatever the client sends onto a supported language.
// It accepts a BCP-47 tag ("pt-BR" → "pt") and is case-insensitive. Anything
// unknown → DefaultLang, never an error: an invalid language preference must not
// deny content to the user.
func NormalizeLang(s string) string {
	// Cut the region subtag and lower the case by hand rather than calling
	// strings.ToLower: this runs on untrusted input and the manual loop makes it
	// obvious that nothing but [a-z] survives into the path.
	base := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '-' || r == '_' {
			break
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		base = append(base, r)
	}
	got := string(base)
	for _, l := range SupportedLangs {
		if l == got {
			return l
		}
	}
	return DefaultLang
}

// Bundle is the content of every language plus the fallback chain declared by the
// source (manifest). Chain is ORDERED and is what guarantees deterministic
// resolution — walking a map would give a different answer on each call.
type Bundle struct {
	ContractVersion string
	Chain           []string
	Cat             map[string]Catalog
}

// resolve returns the real attempt order: the requested language first, then the
// declared chain (without repeating the request).
func (b Bundle) resolve(lang string) []string {
	out := make([]string, 0, len(b.Chain)+1)
	if lang != "" {
		out = append(out, lang)
	}
	for _, l := range b.Chain {
		if l != lang {
			out = append(out, l)
		}
	}
	return out
}

// Catalog returns one language's catalog (empty when absent).
func (b Bundle) Catalog(lang string) Catalog { return b.Cat[lang] }

// hasBody treats a whitespace-only body as ABSENT. The adapter already trims, but
// the domain must not rely on that: if it did, a file holding a single blank line
// would stop the fallback chain and the user would open an empty help drawer
// instead of getting the next language.
func hasBody(it Item) bool { return strings.TrimSpace(it.Body) != "" }

// FAQIn resolves a tab's FAQ by walking the fallback chain. It also returns the
// language ACTUALLY served, so the client can tell it fell back instead of
// assuming the translation exists.
func FAQIn(b Bundle, lang, tab string) (Item, string, bool) {
	for _, l := range b.resolve(lang) {
		if it, ok := b.Cat[l].FAQ[tab]; ok && hasBody(it) {
			return it, l, true
		}
	}
	return Item{}, "", false
}

// InternalIn resolves a deep-dive topic through the fallback chain.
func InternalIn(b Bundle, lang, topic string) (Item, string, bool) {
	for _, l := range b.resolve(lang) {
		if it, ok := b.Cat[l].Internal[topic]; ok && hasBody(it) {
			return it, l, true
		}
	}
	return Item{}, "", false
}

// PublicListIn reports has_faq when ANY language in the chain has the FAQ — the
// help button has to appear whenever there is servable content, even via fallback.
func PublicListIn(b Bundle, lang string) []TabFlag {
	out := make([]TabFlag, 0, len(TabKeys))
	for _, k := range TabKeys {
		_, _, ok := FAQIn(b, lang, k)
		out = append(out, TabFlag{Key: k, HasFAQ: ok})
	}
	return out
}

// InternalListIn lists the deep-dive topics with the title in the resolved
// language. Unioning the keys of ALL languages would be non-deterministic, so the
// list follows InternalKeys and each title comes from the fallback.
func InternalListIn(b Bundle, lang string) []TopicRef {
	out := make([]TopicRef, 0, len(InternalKeys))
	for _, k := range InternalKeys {
		if it, _, ok := InternalIn(b, lang, k); ok {
			out = append(out, TopicRef{Key: k, Title: it.Title})
		}
	}
	return out
}

// TabFlag says, for one tab, whether a public FAQ is registered.
type TabFlag struct {
	Key    string `json:"key"`
	HasFAQ bool   `json:"has_faq"`
}

// TopicRef is the reference to a deep-dive topic (without the body).
type TopicRef struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// ValidTab says whether k is one of the 13 known tabs.
func ValidTab(k string) bool {
	for _, t := range TabKeys {
		if t == k {
			return true
		}
	}
	return false
}

// CanSeeInternal: only platform_admin sees the deep-dive.
func CanSeeInternal(role string) bool { return role == RolePlatformAdmin }

// ---------------------------------------------------------------------------
// Single-language lookups.
//
// Kept after the multi-language work because they are the narrowest expression of
// the audience split, which is the property most worth testing in isolation. The
// shell uses the *In variants; these stay as the single-Catalog contract.
// ---------------------------------------------------------------------------

// PublicList returns the 13 tabs with the FAQ-present flag. Order = TabKeys.
func PublicList(c Catalog) []TabFlag {
	out := make([]TabFlag, 0, len(TabKeys))
	for _, k := range TabKeys {
		_, ok := c.FAQ[k]
		out = append(out, TabFlag{Key: k, HasFAQ: ok})
	}
	return out
}

// FAQ returns a tab's public content. ok=false when the tab has no FAQ.
// An invalid tab is the shell's responsibility (ValidTab) — this is just a lookup.
func FAQ(c Catalog, tab string) (Item, bool) {
	it, ok := c.FAQ[tab]
	return it, ok
}

// InternalList returns the deep-dive topics (key+title), without the body, in
// InternalKeys order. It used to iterate the map, which returned a different order
// on every call and made the list move on screen with nothing having changed.
func InternalList(c Catalog) []TopicRef {
	out := make([]TopicRef, 0, len(c.Internal))
	for _, k := range InternalKeys {
		if it, ok := c.Internal[k]; ok {
			out = append(out, TopicRef{Key: k, Title: it.Title})
		}
	}
	return out
}

// Internal returns a deep-dive topic's content. ok=false when it does not exist.
func Internal(c Catalog, topic string) (Item, bool) {
	it, ok := c.Internal[topic]
	return it, ok
}
