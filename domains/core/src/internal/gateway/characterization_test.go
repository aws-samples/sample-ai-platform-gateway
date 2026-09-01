// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Handler tests after the extraction of the pure domain
// (spec intelligent-cost-routing, tasks 1 and 7.7).
//
// HISTORY: this file was born as characterization — it captured the CURRENT behavior
// of cheapest(), buildChain(), defaultModel() and estimateCost(), including where it
// was wrong, so that an unintended change during the extraction would be caught.
// Once that job was done, the cases that documented a bug were REWRITTEN here with
// the correct behavior, and the four bugs became regression tests:
//
//	BUG-1  cheapest() summed Input+Output as if they carried the same weight
//	       → the cost is now weighted by real tokens (internal/routing).
//	BUG-2  there was no capability filter (tool use went to an incapable model)
//	       → eligibility is now a hard layer, tested in routing/decide_test.go.
//	BUG-3  a price of zero won the optimization
//	       → a price of zero is now "unknown" and leaves the optimization.
//	BUG-4  the provider's cache tokens were discarded
//	       → they now enter the split and the realized cost.
//
// What remains here is what is still the handler's responsibility:
// buildChain(), defaultModel() and the config → domain translation.
package gateway

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/aiplat/core/internal/routing"
)

// --- helpers -----------------------------------------------------------------

func cfg(routes map[string]Route, pricing map[string]routing.PriceHistory, mutate ...func(*Config)) *Config {
	c := &Config{Routing: routes, Pricing: pricing}
	for _, m := range mutate {
		m(c)
	}
	return c
}

func route(provider, id string, fallback ...string) Route {
	return Route{Provider: provider, ProviderModelID: id, Fallback: fallback}
}

// routeCap is a route with declared capabilities — what the console now writes.
func routeCap(provider, id string, toolUse bool, ctx int, tier string, fallback ...string) Route {
	r := route(provider, id, fallback...)
	r.Capabilities = routing.Capabilities{ToolUse: toolUse, ContextWindow: ctx, Tier: tier}
	return r
}

func price(in, out float64) routing.PriceHistory {
	return routing.PriceHistory{{Standard: routing.Layer{Input: in, Output: out}}}
}

func chainNames(steps []step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.name)
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var refNow = time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)

// --- BUG-1 and BUG-2: the decision is now weighted and respects capability ----

func TestDecisao_ToolUseNaoVaiParaModeloIncapaz(t *testing.T) {
	c := cfg(
		map[string]Route{
			"claude-sonnet": routeCap("bedrock", "us.anthropic.claude-sonnet-5", true, 200_000, "frontier"),
			"nova-micro":    routeCap("bedrock", "us.amazon.nova-micro-v1:0", false, 128_000, "fast"),
		},
		map[string]routing.PriceHistory{
			"claude-sonnet": price(0.003, 0.015),
			"nova-micro":    price(0.000035, 0.00014),
		},
		func(c *Config) { c.AutoCheapest = true },
	)
	shape := routing.RequestShape{InputTokens: 400, MaxOutputTokens: 256, HasTools: true}
	dec, err := routing.Decide(candidates(c), policyFor(c, ""), nil, nil, shape, refNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Model != "claude-sonnet" {
		t.Errorf("chosen = %q, expected claude-sonnet (the only one with tool use)", dec.Model)
	}
}

func TestDecisao_CustoPonderadoPorTokensReais(t *testing.T) {
	// muito-input is expensive per OUTPUT token and cheap on input; muito-output is
	// the opposite. With a load dominated by INPUT, the winner has to be muito-input.
	// The naive sum (input+output) picked the other one.
	c := cfg(
		map[string]Route{
			"muito-input":  routeCap("bedrock", "a", true, 0, "fast"),
			"muito-output": routeCap("bedrock", "b", true, 0, "fast"),
		},
		map[string]routing.PriceHistory{
			"muito-input":  price(0.0001, 0.030),
			"muito-output": price(0.0200, 0.001),
		},
		func(c *Config) { c.AutoCheapest = true },
	)
	// 20,000 input tokens, 100 output → input dominates.
	shape := routing.RequestShape{InputTokens: 20_000, MaxOutputTokens: 100}
	dec, err := routing.Decide(candidates(c), policyFor(c, ""), nil, nil, shape, refNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Model != "muito-input" {
		t.Errorf("chosen = %q, expected muito-input (the naive sum would pick muito-output)", dec.Model)
	}
}

func TestDecisao_PrecoZeroNaoVence(t *testing.T) {
	c := cfg(
		map[string]Route{
			"nova-micro": routeCap("bedrock", "us.amazon.nova-micro-v1:0", true, 0, "fast"),
			"sem-preco":  routeCap("bedrock", "modelo-novo", true, 0, "fast"),
		},
		map[string]routing.PriceHistory{
			"nova-micro": price(0.000035, 0.00014),
			"sem-preco":  price(0, 0), // the wizard's default
		},
		func(c *Config) { c.AutoCheapest = true },
	)
	shape := routing.RequestShape{InputTokens: 400, MaxOutputTokens: 256}
	dec, err := routing.Decide(candidates(c), policyFor(c, ""), nil, nil, shape, refNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Model != "nova-micro" {
		t.Errorf("chosen = %q, expected nova-micro", dec.Model)
	}
}

// --- BUG-4: cache tokens enter the cost --------------------------------------

func TestRealizedCost_TokensDeCacheReduzemOCusto(t *testing.T) {
	c := cfg(
		map[string]Route{"m": routeCap("bedrock", "id", true, 0, "fast")},
		map[string]routing.PriceHistory{"m": price(0.003, 0.015)},
	)
	caps := c.Routing["m"].Capabilities

	// The comparison has to keep the input TOTAL equal (1,900 tokens). Under the
	// EXCLUSIVE convention (the default), `tin` counts only the NON-cached ones — so
	// tin=1000 with cacheRead=900 is 1,900 tokens in total, and comparing that with
	// tin=1000 alone would compare different loads.
	semCache := result{tin: 1900, tout: 100}
	comCache := result{tin: 1000, tout: 100, cacheRead: 900, cacheConv: "reported"}

	custoSem, savedSem, _ := realizedCost(c, "m", caps, semCache, refNow)
	custoCom, savedCom, _ := realizedCost(c, "m", caps, comCache, refNow)

	if !(custoCom < custoSem) {
		t.Errorf("with cache (%v) should cost less than without (%v) for the same input total", custoCom, custoSem)
	}
	if savedSem != 0 {
		t.Errorf("without cache there are no cache savings: %v", savedSem)
	}
	if savedCom <= 0 {
		t.Errorf("with cache it should record savings: %v", savedCom)
	}
}

// The counting convention changes the cost — that is the risk declared in the design.
// This test exists so the difference is EXPLICIT in the code, not a surprise later.
func TestRealizedCost_ConvencaoDeContagemMudaOCusto(t *testing.T) {
	c := cfg(
		map[string]Route{
			"excl": routeCap("bedrock", "id", true, 0, "fast"),
			"incl": routeCap("bedrock", "id", true, 0, "fast"),
		},
		map[string]routing.PriceHistory{"excl": price(0.003, 0.015), "incl": price(0.003, 0.015)},
	)
	// Same provider response, different conventions.
	res := result{tin: 1000, tout: 100, cacheRead: 900, cacheConv: "reported"}

	capsExcl := routing.Capabilities{CacheTokensInclusive: false}
	capsIncl := routing.Capabilities{CacheTokensInclusive: true}

	custoExcl, _, _ := realizedCost(c, "excl", capsExcl, res, refNow)
	custoIncl, _, _ := realizedCost(c, "incl", capsIncl, res, refNow)

	// Exclusive: 1000 non-cached + 900 cached. Inclusive: 100 + 900.
	if !(custoIncl < custoExcl) {
		t.Errorf("inclusive (%v) should cost less than exclusive (%v)", custoIncl, custoExcl)
	}
}

func TestRealizedCost_PrecoDesconhecidoNaoInventaCusto(t *testing.T) {
	c := cfg(
		map[string]Route{"m": routeCap("bedrock", "id", true, 0, "fast")},
		map[string]routing.PriceHistory{"m": price(0, 0)},
	)
	cost, saved, st := realizedCost(c, "m", c.Routing["m"].Capabilities, result{tin: 1000, tout: 100}, refNow)
	if cost != 0 || saved != 0 {
		t.Errorf("unknown price: cost=%v saved=%v, expected 0/0", cost, saved)
	}
	if st != routing.PricingUnknown {
		t.Errorf("status = %q, expected unknown", st)
	}
}

// --- Pricing: the old shape written today remains valid ----------------------

func TestConfig_AceitaPricingNaFormaAntiga(t *testing.T) {
	// Exactly what is stored in the orgs' config today.
	raw := `{"pricing":{"nova-micro":{"input":0.000035,"output":0.00014}},
	         "routing":{"nova-micro":{"provider":"bedrock","provider_model_id":"us.amazon.nova-micro-v1:0"}}}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	tier, st := routing.SelectPrice(c.Pricing["nova-micro"], refNow)
	if st == routing.PricingUnknown {
		t.Fatal("the old shape should remain valid with no migration")
	}
	if math.Abs(tier.Standard.Input-0.000035) > 1e-12 {
		t.Errorf("Standard.Input = %v", tier.Standard.Input)
	}
	// Absent capabilities: conservative — incapable of tool use.
	if c.Routing["nova-micro"].Capabilities.ToolUse {
		t.Error("an absent capability should count as false")
	}
}

func TestConfig_AceitaPricingComVigencia(t *testing.T) {
	raw := `{"pricing":{"claude-sonnet":[
	   {"effective_from":"2026-08-01","standard":{"input":0.002,"output":0.010}},
	   {"effective_from":"2026-09-01","standard":{"input":0.003,"output":0.015}}]}}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// On 2026-08-09 the August effective date applies.
	tier, _ := routing.SelectPrice(c.Pricing["claude-sonnet"], refNow)
	if math.Abs(tier.Standard.Input-0.002) > 1e-12 {
		t.Errorf("in August: Standard.Input = %v, expected 0.002", tier.Standard.Input)
	}
	// On 2026-09-01 the new one takes over, with no redeploy.
	set, _ := routing.SelectPrice(c.Pricing["claude-sonnet"], time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if math.Abs(set.Standard.Input-0.003) > 1e-12 {
		t.Errorf("in September: Standard.Input = %v, expected 0.003", set.Standard.Input)
	}
}

// --- Budget: both names of the monthly ceiling must apply --------------------

// Regression for a real bug: the router read only "monthly_usd", but the console
// writes "limit_usd". The effect was silent and serious — every budget the customer
// set was ignored by the gateway, meaning governance did not hold.
func TestConfig_BudgetAceitaOsDoisNomesDoTeto(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want float64
	}{
		{"limit_usd (console)", `{"budget":{"limit_usd":250,"action":"block"}}`, 250},
		{"monthly_usd (old seed)", `{"budget":{"monthly_usd":100,"action":"alert"}}`, 100},
		// In the scope merge both can coexist (the org seeded monthly_usd, the team
		// just configured limit_usd). The most specific one wins.
		{"both: limit_usd prevails", `{"budget":{"monthly_usd":100,"limit_usd":5,"action":"block"}}`, 5},
		{"neither: no ceiling", `{"budget":{"action":"block"}}`, 0},
		{"no budget: no ceiling", `{}`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			if err := json.Unmarshal([]byte(tc.raw), &c); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got := c.Budget.Limit(); math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("Limit() = %v, expected %v", got, tc.want)
			}
		})
	}
}

// --- buildChain(): still the handler's responsibility -------------------------

func TestBuildChain(t *testing.T) {
	routes := map[string]Route{
		"a": route("bedrock", "ida", "b"),
		"b": route("bedrock", "idb", "c"),
		"c": route("bedrock", "idc"),
	}

	tests := []struct {
		name   string
		c      *Config
		chosen string
		want   []string
	}{
		{
			name:   "with no model_order it uses the route's fallback",
			c:      cfg(routes, nil),
			chosen: "a",
			want:   []string{"a", "b"},
		},
		{
			name: "model_order overrides the route's fallback",
			c: cfg(routes, nil, func(c *Config) {
				c.ModelOrder = []string{"a", "c", "b"}
			}),
			chosen: "a",
			want:   []string{"a", "c", "b"},
		},
		{
			name: "chosen at the end of the order has no fallback",
			c: cfg(routes, nil, func(c *Config) {
				c.ModelOrder = []string{"b", "c", "a"}
			}),
			chosen: "a",
			want:   []string{"a"},
		},
		{
			name: "chosen outside the order falls back to the route's fallback",
			c: cfg(routes, nil, func(c *Config) {
				c.ModelOrder = []string{"b", "c"}
			}),
			chosen: "a",
			want:   []string{"a", "b"},
		},
		{
			name: "model_order respects allowed_models",
			c: cfg(routes, nil, func(c *Config) {
				c.ModelOrder = []string{"a", "b", "c"}
				c.AllowedModels = []string{"a", "c"}
			}),
			chosen: "a",
			want:   []string{"a", "c"},
		},
		{
			name: "a model in the order missing from routing is ignored",
			c: cfg(routes, nil, func(c *Config) {
				c.ModelOrder = []string{"a", "inexistente", "c"}
			}),
			chosen: "a",
			want:   []string{"a", "c"},
		},
		{
			// Auto-cheapest reorders the chain by PRICE: chosen (the cheapest) first,
			// then the next cheapest in the list. With input=0 the cost is ~output:
			// b(0.003) < c(0.015) < a(0.030).
			name: "auto-cheapest orders the chain by price",
			c: cfg(routes, map[string]routing.PriceHistory{
				"a": price(0.010, 0.030),
				"b": price(0.001, 0.003),
				"c": price(0.005, 0.015),
			}, func(c *Config) {
				c.ModelOrder = []string{"a", "b", "c"}
				c.AutoCheapest = true
			}),
			chosen: "b",
			want:   []string{"b", "c", "a"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := chainNames(buildChain(tc.c, policyFor(tc.c, ""), tc.chosen, tc.c.Routing[tc.chosen], routing.RequestShape{}, time.Now()))
			if !eqStrings(got, tc.want) {
				t.Errorf("buildChain() = %v, expected %v", got, tc.want)
			}
		})
	}
}

// --- defaultModel(): still the handler's responsibility -----------------------

func TestDefaultModel(t *testing.T) {
	routes := map[string]Route{
		"a": route("bedrock", "ida"),
		"b": route("bedrock", "idb"),
	}

	tests := []struct {
		name string
		c    *Config
		want string
	}{
		{
			name: "first in model_order",
			c:    cfg(routes, nil, func(c *Config) { c.ModelOrder = []string{"b", "a"} }),
			want: "b",
		},
		{
			name: "skips what is not in allowed_models",
			c: cfg(routes, nil, func(c *Config) {
				c.ModelOrder = []string{"b", "a"}
				c.AllowedModels = []string{"a"}
			}),
			want: "a",
		},
		{
			name: "skips what does not exist in routing",
			c:    cfg(routes, nil, func(c *Config) { c.ModelOrder = []string{"inexistente", "a"} }),
			want: "a",
		},
		{
			name: "with no model_order it falls back to default_model",
			c:    cfg(routes, nil, func(c *Config) { c.DefaultModel = "b" }),
			want: "b",
		},
		{
			name: "a default_model outside allowed_models does not count",
			c: cfg(routes, nil, func(c *Config) {
				c.DefaultModel = "b"
				c.AllowedModels = []string{"a"}
			}),
			want: "",
		},
		{
			name: "no order and no default returns empty",
			c:    cfg(routes, nil),
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultModel(tc.c); got != tc.want {
				t.Errorf("defaultModel() = %q, expected %q", got, tc.want)
			}
		})
	}
}

// --- allowed() ---------------------------------------------------------------

func TestAllowed(t *testing.T) {
	tests := []struct {
		name  string
		allow []string
		model string
		want  bool
	}{
		{"an empty list allows everything", nil, "qualquer", true},
		{"an explicit empty list allows everything", []string{}, "qualquer", true},
		{"present in the list", []string{"a", "b"}, "a", true},
		{"absent from the list", []string{"a", "b"}, "c", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{AllowedModels: tc.allow}
			if got := c.allowed(tc.model); got != tc.want {
				t.Errorf("allowed(%q) = %v, expected %v", tc.model, got, tc.want)
			}
		})
	}
}

// --- hasImage(): multimodal content detection --------------------------------

func TestHasImage(t *testing.T) {
	textOnly := []chatMsg{{Role: "user", Content: json.RawMessage(`"só texto"`)}}
	if hasImage(textOnly) {
		t.Error("plain text has no image")
	}

	multimodal := []chatMsg{{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"o que é isto?"},{"type":"image_url","image_url":{"url":"data:..."}}]`)}}
	if !hasImage(multimodal) {
		t.Error("it should detect image_url")
	}

	arrayTextOnly := []chatMsg{{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)}}
	if hasImage(arrayTextOnly) {
		t.Error("a text-only array has no image")
	}

	if hasImage(nil) {
		t.Error("nil has no image")
	}
}

// bedrockCacheTokens() was moved to internal/adapters/bedrock along with the rest of
// the Bedrock adapter; its test lives there now (bedrock_test.go).

// --- images()/decodeDataURLImage: multimodal content actually reaches the provider ---
//
// Regression for the bug hasImage's own tests did not catch: routing eligibility
// used hasImage to ROUTE multimodal requests to capable models, but convertMessages
// (bedrock adapter) never read the image bytes — it only ever sent m.Text. A request
// with an image looked routed correctly and still silently reached the model as
// text-only. These tests pin the extraction that closes that gap.
func TestChatMsgImages(t *testing.T) {
	png1x1 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	m := chatMsg{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"o que é isto?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png1x1 + `"}}]`)}
	imgs := m.images()
	if len(imgs) != 1 {
		t.Fatalf("images() = %d parts, want 1", len(imgs))
	}
	if imgs[0].Format != "png" {
		t.Errorf("format = %q, want png", imgs[0].Format)
	}
	if len(imgs[0].Bytes) == 0 {
		t.Error("bytes should not be empty — the point of this path is sending the actual image")
	}

	textOnly := chatMsg{Role: "user", Content: json.RawMessage(`"só texto"`)}
	if imgs := textOnly.images(); imgs != nil {
		t.Errorf("images() on plain text = %v, want nil", imgs)
	}

	remoteURL := chatMsg{Role: "user", Content: json.RawMessage(
		`[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]`)}
	if imgs := remoteURL.images(); imgs != nil {
		t.Errorf("a remote (non-data) URL should be dropped, not sent as bytes; got %v", imgs)
	}
}

func TestDecodeDataURLImage(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		wantOK bool
		format string
	}{
		{"valid png", "data:image/png;base64,QUJD", true, "png"},
		{"valid jpeg", "data:image/jpeg;base64,QUJD", true, "jpeg"},
		{"remote url", "https://example.com/x.png", false, ""},
		{"missing base64 marker", "data:image/png,QUJD", false, ""},
		{"non-base64 encoding", "data:image/png;utf8,abc", false, ""},
		{"invalid base64 payload", "data:image/png;base64,not-valid-base64!!", false, ""},
		{"empty", "", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			format, data, ok := decodeDataURLImage(c.url)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && format != c.format {
				t.Errorf("format = %q, want %q", format, c.format)
			}
			if ok && len(data) == 0 {
				t.Error("decoded bytes should not be empty on a valid data URL")
			}
		})
	}
}

// --- withText(): the PII mask must not silently drop images ------------------
//
// Regression: applyGuardrails rebuilds every message via withText when mask_pii is
// on. The old withText always collapsed Content to a bare JSON string, so on a
// multimodal message (text + image_url) the image_url part was discarded before
// the message ever reached toPortsMessages/the provider adapter — independent of
// the Bedrock imageBlocks fix, because the image was already gone by then.
func TestWithTextPreservesImages(t *testing.T) {
	m := chatMsg{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"meu email é a@b.com"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]`)}
	masked := m.withText("meu email é [REDACTED]")

	if got := masked.text(); got != "meu email é [REDACTED]" {
		t.Errorf("text() = %q, want the masked text", got)
	}
	imgs := masked.images()
	if len(imgs) != 1 {
		t.Fatalf("withText dropped the image: images() = %d parts, want 1", len(imgs))
	}
	if imgs[0].Format != "png" {
		t.Errorf("format = %q, want png (image part must survive untouched)", imgs[0].Format)
	}

	// Plain-string content (the common case) keeps the old, simpler behavior.
	plain := chatMsg{Role: "user", Content: json.RawMessage(`"olá"`)}
	if got := plain.withText("oi").text(); got != "oi" {
		t.Errorf("text() = %q, want %q", got, "oi")
	}
}
