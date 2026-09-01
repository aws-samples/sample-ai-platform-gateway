// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package gateway is the Core's ORCHESTRATION SHELL: it drives one request from
// the neutral inbound boundary (httpapi.Request) through auth, config, guardrails,
// limits, the pure routing decision, cache, provider dispatch and telemetry — and
// assembles the OpenAI-dialect response.
//
// It is shell, not domain: it does IO through the ports and adapters, and delegates
// every decision to internal/routing. It knows NOTHING about Lambda (that is
// internal/awslambda's job) nor about how the adapters are constructed (that is
// cmd/router/main.go's job, via Wire).
//
// Slice (c) of hexagonal-refactor task 7.4: `handle` and its helpers moved here
// verbatim from cmd/router, which is now dependency wiring only (R1.4).
package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiplat/core/internal/adapters/anthropic"
	"github.com/aiplat/core/internal/adapters/bedrock"
	"github.com/aiplat/core/internal/adapters/ddbcache"
	"github.com/aiplat/core/internal/adapters/ddbhints"
	"github.com/aiplat/core/internal/adapters/google"
	"github.com/aiplat/core/internal/adapters/openaicompat"
	"github.com/aiplat/core/internal/httpapi"
	"github.com/aiplat/core/internal/ports"
	"github.com/aiplat/core/internal/routing"
)

type Route struct {
	Provider        string   `json:"provider"`
	ProviderModelID string   `json:"provider_model_id"`
	BaseURL         string   `json:"base_url"`
	APIKeySecret    string   `json:"api_key_secret"`
	Fallback        []string `json:"fallback"`

	// ModelID declares WHICH WEIGHTS this route serves (e.g. "openai/gpt-5.2").
	// Routes sharing it are the same model, so swapping between them carries no
	// quality risk — that is what turns provider failover and provider arbitrage
	// into safe operations. Empty means "not declared", never inferred.
	ModelID string `json:"model_id,omitempty"`
	// Aggregator marks a provider that routes internally to varying upstreams and
	// may serve a different version or quantization between requests. Such a route
	// never joins an identity group, because the no-quality-risk promise would be
	// false there.
	Aggregator bool `json:"aggregator,omitempty"`

	// BYO Bedrock: role in the CUSTOMER's account that we assume at runtime.
	// The spend and the data stay in their account. Set in the org's config.
	RoleARN    string `json:"role_arn,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	Region     string `json:"region,omitempty"`

	// PromptCache turns on the provider's prompt caching for this route (today only
	// Bedrock). Opt-in (default false): the cache-write charges a premium and only
	// pays off when the system prefix repeats across requests. The adapter marks the
	// end of the system block with a cache point when this is true.
	PromptCache bool `json:"prompt_cache,omitempty"`

	// Capabilities feeds the routing eligibility filter. Absence counts as
	// incapable of tool use and multimodal — deliberately conservative, so we do
	// not repeat the bug of sending tool use to a model that returns
	// `arguments:{}`.
	Capabilities routing.Capabilities `json:"capabilities"`
}
type Budget struct {
	// Two names for the SAME monthly ceiling, for contract compatibility:
	//   - monthly_usd: name used by the original plan seed (govcore.PlanLimits).
	//   - limit_usd:   name the CONSOLE writes when setting a budget per scope.
	// Ignoring limit_usd made every budget set from the console be silently
	// discarded by the gateway (governance did not hold). Limit() resolves the
	// precedence.
	MonthlyUSD float64 `json:"monthly_usd"`
	LimitUSD   float64 `json:"limit_usd"`
	Action     string  `json:"action"` // alert | degrade | block
}

// Limit returns the effective monthly ceiling. The most SPECIFIC one wins: when
// both arrive from the scope merge (org sets monthly_usd, team sets limit_usd),
// limit_usd is what the customer just configured — so it prevails.
func (b *Budget) Limit() float64 {
	if b == nil {
		return 0
	}
	if b.LimitUSD > 0 {
		return b.LimitUSD
	}
	return b.MonthlyUSD
}

type Limits struct {
	RPM int `json:"requests_per_minute"`
	TPM int `json:"tokens_per_minute"`
}

// GuardrailPolicy: content controls applied BEFORE calling the provider.
// The deterministic ones (mask/secret/no_store) are real; injection is heuristic.
type GuardrailPolicy struct {
	MaskPII        bool `json:"mask_pii"`        // masks e-mail/CPF/card/phone
	BlockSecrets   bool `json:"block_secrets"`   // blocks if the prompt contains a secret/key
	BlockInjection bool `json:"block_injection"` // prompt injection heuristic
	NoStore        bool `json:"no_store"`        // turns off the response cache (zero retention)
}
type Config struct {
	AutoCheapest bool `json:"auto_cheapest"`
	CacheTTL     int  `json:"cache_ttl"`
	// CacheKeyMode: "exact" (default) or "canonical". In canonical mode, trivial
	// text variations (case/accent/whitespace/punctuation) collide on the key — it
	// raises the hit rate on FAQ traffic. Inherited by scope (global→org→team→app).
	CacheKeyMode string `json:"cache_key_mode,omitempty"`
	// SemanticCache turns on the SEMANTIC cache (approximate match by embedding) on
	// top of the exact/canonical one. Opt-in (default false): it admits false
	// positives and adds an embedding call on the MISS. SemanticThreshold is the
	// cosine similarity floor (0 → SemDefaultThreshold). Both are inherited by
	// scope like the rest of the config.
	SemanticCache     bool             `json:"semantic_cache,omitempty"`
	SemanticThreshold float64          `json:"semantic_threshold,omitempty"`
	Routing           map[string]Route `json:"routing"`
	// Pricing accepts the old {input,output} shape and the new one with tiers and
	// effective dates — PriceHistory.UnmarshalJSON handles both, with no data
	// migration.
	Pricing       map[string]routing.PriceHistory `json:"pricing"`
	AllowedModels []string                        `json:"allowed_models,omitempty"`

	// FeaturePolicy: quality policy per feature (acceptable tiers/models and
	// economy mode). The intent is DECLARED by the customer, never inferred from
	// the prompt.
	FeaturePolicy map[string]FeaturePolicy `json:"feature_policy,omitempty"`
	// Credits: credit balance declared per provider (key = provider).
	Credits map[string]CreditDecl `json:"credits,omitempty"`

	// Bundles: named attempt orders in layers, referenced by feature_policy.bundle.
	// Declared once per org and reused across features, so adding a provider means
	// editing one bundle instead of every feature's flat model list.
	Bundles map[string]routing.Bundle `json:"bundles,omitempty"`

	// ModelOrder is the model priority order, set by drag and drop in the console.
	// It defines the default model (the 1st allowed one) and the fallback chain
	// (the following ones). It is a LIST on purpose: lists replace on inheritance,
	// so an org reorders without materializing the global catalog.
	ModelOrder []string `json:"model_order,omitempty"`
	// DefaultModel: used when the request carries no `model`.
	// Only applies when there is no ModelOrder.
	DefaultModel string  `json:"default_model,omitempty"`
	Budget       *Budget `json:"budget,omitempty"`
	Limits       *Limits `json:"rate_limits,omitempty"`
	// Org lifecycle (written by the backoffice). "suspended" = kill-switch.
	Status string `json:"status,omitempty"`

	// Content guardrails (policy per scope, written by the console).
	Guardrails *GuardrailPolicy `json:"guardrails,omitempty"`

	// Scope (config pk) that DEFINED each policy. The enforcement counter uses that
	// scope: a limit set on the org counts for the whole org; set on the team, it
	// counts only for that team.
	budgetScope string `json:"-"`
	limitsScope string `json:"-"`
}

// FeaturePolicy is the quality policy declared for a feature.
// Claude Code's mental model: a role declared by kind of work, never a classifier
// of prompt complexity.
type FeaturePolicy struct {
	Tiers       []string `json:"tiers,omitempty"`
	Models      []string `json:"models,omitempty"`
	EconomyMode bool     `json:"economy_mode,omitempty"`
	Escalate    *bool    `json:"escalate,omitempty"` // nil = follows EconomyMode
	// Swap is the declared ceiling on how far a route substitution may go:
	// "same_model_only", "allow_equivalent" or "allow_downgrade". Empty keeps the
	// pre-feature behavior (any substitution permitted), so no existing org changes
	// behavior. It binds all three substitution paths, budget degrade included.
	Swap string `json:"swap,omitempty"`
	// Bundle names an entry of the org's `bundles` map: the declared attempt order
	// in layers for this feature. Empty keeps model_order as the only preference.
	Bundle string `json:"bundle,omitempty"`
	// Canary sends a declared fraction of this feature's traffic to a candidate
	// route, so cost, latency and error rate can be compared on the customer's own
	// prompts. nil disables it.
	Canary *routing.Canary `json:"canary,omitempty"`
}

// CreditDecl is the provider credit DECLARED manually by the customer.
// We do not read their invoice: that would require Billing/Cost Explorer access,
// far more invasive than today's bedrock:InvokeModel.
type CreditDecl struct {
	AmountUSD    float64 `json:"amount_usd"`
	CorrectedUSD float64 `json:"corrected_remaining_usd,omitempty"`
	ExpiresAt    string  `json:"expires_at,omitempty"` // YYYY-MM-DD
}

// allowed says whether the model is permitted in the effective scope (an empty
// list means everything is allowed).
func (c *Config) allowed(model string) bool {
	if len(c.AllowedModels) == 0 {
		return true
	}
	for _, m := range c.AllowedModels {
		if m == model {
			return true
		}
	}
	return false
}

// State adapters (hexagonal-refactor, task 4). Every infrastructure concern lives
// behind its port in internal/adapters/*; the globals here are the production
// instances, wired in main(). The helpers in this file (loadConfig, bump,
// readCounter, emitUsage, getSecret, authResolve, cache) are a thin shell
// delegating to them — the handler's decision logic does not change.
var (
	// bedrockPool resolves/caches Bedrock clients (pooled or cross-account role).
	bedrockPool *bedrock.Pool

	// Typed as PORTS (not the concrete adapters): that is what allows injecting an
	// in-memory double in the orchestration test (R3.2) and what keeps the handler
	// depending only on the boundary, not on the technology.
	configStore ports.ConfigStore
	cacheStore  ports.Cache
	limitsStore ports.LimitsStore
	usageSink   ports.UsageSink
	secretStore ports.SecretStore
	keyStore    ports.KeyStore

	// hintsReader reads Observability's Routing_Hints as a contract, with a short
	// cache and fallback. Empty/nil = the decision falls back to the heuristic and
	// keeps serving.
	hintsReader *ddbhints.Reader

	// deploymentOrg is this deployment's single org (DEPLOYMENT_ORG). Used only to
	// derive the default BYO Bedrock ExternalID (see callProvider) — never for
	// config scoping, which configStore already handles internally.
	deploymentOrg string

	// Semantic cache (opt-in): embedder produces the question's vector and semStore
	// holds the per-org index (same table as the cache). nil = feature unavailable
	// (e.g. under test), and the handler simply skips it — graceful degradation.
	embedder ports.Embedder
	semStore *ddbcache.Store

	httpc = &http.Client{Timeout: 45 * time.Second}
)

// semIndexCap caps how many entries an org's semantic index keeps (FIFO).
// It keeps the item well under DynamoDB's 400KB limit and the linear search cheap.
const semIndexCap = 200

// semDefaultStoreTTL is the TTL used to store responses when the semantic cache is
// ON but the exact response cache is "off" (ttl=0). Without it the semantic cache
// would have nothing to reuse (the response would expire immediately). It lets the
// semantic cache work AUTONOMOUSLY, without requiring the exact cache to be turned
// on first.
const semDefaultStoreTTL = 3600

// effectiveCacheTTL resolves the storage TTL: normally the config's cache_ttl; but
// if the semantic cache is on and the exact one is off, it falls back to the
// default so the semantic index has live responses to serve.
func effectiveCacheTTL(c *Config) int {
	if c.SemanticCache && c.CacheTTL <= 0 {
		return semDefaultStoreTTL
	}
	return c.CacheTTL
}

func envConfig() *Config {
	c := &Config{CacheTTL: 3600, Routing: map[string]Route{}, Pricing: map[string]routing.PriceHistory{}}
	if v := os.Getenv("MODEL_ROUTING"); v != "" {
		json.Unmarshal([]byte(v), &c.Routing)
	}
	if v := os.Getenv("PRICING_TABLE"); v != "" {
		json.Unmarshal([]byte(v), &c.Pricing)
	}
	return c
}

// loadConfig resolves the EFFECTIVE config for team/app, delegating the table
// read + 15s cache + scope merge to the ddbconfig adapter. Converting the effective
// map into the (handler's) Config type and the defaults stay here — that is the
// shell→domain translation. Fallback: if Governance is unavailable, the base of
// environment defaults prevails (the adapter only merges whatever the table
// returned).
// Single-org model: org parameter removed from hierarchy.
func loadConfig(ctx context.Context, team, app string) *Config {
	// Base: environment defaults (fallback). A fresh map per call — the merge mutates it.
	base := map[string]interface{}{}
	if b, err := json.Marshal(envConfig()); err == nil {
		json.Unmarshal(b, &base)
	}

	merged, budgetScope, limitsScope := configStore.Effective(ctx, team, app, base)

	c := &Config{}
	if b, err := json.Marshal(merged); err == nil {
		json.Unmarshal(b, c)
	}
	c.budgetScope, c.limitsScope = budgetScope, limitsScope
	if c.CacheTTL == 0 {
		c.CacheTTL = 3600
	}
	if c.Routing == nil {
		c.Routing = map[string]Route{}
	}
	if c.Pricing == nil {
		c.Pricing = map[string]routing.PriceHistory{}
	}
	return c
}

// candidates translates the effective config into the pure domain's vocabulary.
// It is the only bridge: the domain knows neither Config nor Route.
func candidates(c *Config) []routing.Candidate {
	out := make([]routing.Candidate, 0, len(c.Routing))
	// Stable order so the decision is reproducible (a Go map has no order).
	names := make([]string, 0, len(c.Routing))
	for m := range c.Routing {
		names = append(names, m)
	}
	sort.Strings(names)
	for _, m := range names {
		r := c.Routing[m]
		out = append(out, routing.Candidate{
			Model:      m,
			Provider:   r.Provider,
			Caps:       r.Capabilities,
			Prices:     c.Pricing[m],
			ModelID:    r.ModelID,
			Aggregator: r.Aggregator,
		})
	}
	return out
}

// policyFor assembles the domain's Policy from the effective config and the feature.
func policyFor(c *Config, feature string) routing.Policy {
	p := routing.Policy{
		AllowedModels:  c.AllowedModels,
		ModelOrder:     c.ModelOrder,
		AutoCheapest:   c.AutoCheapest,
		DefaultOutTok:  defaultOutTokens,
		MinHintSamples: minHintSamples(),
	}
	if fp, ok := c.FeaturePolicy[feature]; ok {
		p.FeatureTiers, p.FeatureModels, p.EconomyMode = fp.Tiers, fp.Models, fp.EconomyMode
		p.Swap = fp.Swap
		// A feature naming a bundle that does not exist gets NO bundle, not an
		// error: the config's default order still serves. Refusing traffic over a
		// misspelled bundle name would be the worst possible trade.
		if fp.Bundle != "" {
			if b, ok := c.Bundles[fp.Bundle]; ok {
				b.Name = fp.Bundle
				p.Bundle = &b
			}
		}
	}
	return p
}

// creditState resolves the estimated credit balance per provider.
//
// The balance is a LOWER BOUND on real consumption: our ledger only sees what goes
// through the gateway, and provider credit is consumed by the deployment's whole account.
// Single-org model: org parameter removed from counter keys.
func creditState(ctx context.Context, c *Config, now time.Time) *routing.CreditState {
	if len(c.Credits) == 0 {
		return nil
	}
	st := &routing.CreditState{ByProvider: map[string]routing.Credit{}}
	for provider, d := range c.Credits {
		var exp time.Time
		if d.ExpiresAt != "" {
			if t, err := time.Parse("2006-01-02", d.ExpiresAt); err == nil {
				exp = t.UTC()
			}
		}
		consumed := readCounter(ctx, "CREDIT#"+provider, "spend")
		st.ByProvider[provider] = routing.Credit{
			RemainingUSD: routing.Remaining(d.AmountUSD, d.CorrectedUSD, consumed),
			ExpiresAt:    exp,
		}
	}
	return st
}

// getSecret fetches the credential from the deployment's secret store. Delegates to
// the secrets adapter (cache by name + 5 min TTL). "" = no credential.
// Single-org model: org parameter removed.
func getSecret(ctx context.Context, name string) string {
	return secretStore.Get(ctx, name)
}

type result struct {
	text       string
	tin        int
	tout       int
	toolCalls  []toolCall // tool_calls returned by the model (function calling)
	stopReason string     // "end_turn", "tool_use", etc.

	// Provider prompt cache counters. They used to be DISCARDED: with prompt caching
	// on (~90% discount on the read), cost and saved_usd came out wrong in both
	// directions — in a product whose differentiator is auditable cost attribution.
	cacheRead  int
	cacheWrite int
	// cacheConv distinguishes "the provider reported zero" from "the provider does
	// not report". Treating both as zero would hide real savings.
	cacheConv string // ports.CacheCountersReported | ports.CacheCountersAbsent
}

// --- Tool Use (Function Calling) in the OpenAI dialect ---
// The client sends "tools" (an array of function definitions) and the model may
// answer with "tool_calls" asking the client to run functions.

// toolDef represents a tool in the OpenAI format (what the client sends).
type toolDef struct {
	Type     string `json:"type"` // always "function"
	Function struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description,omitempty"`
		Parameters  map[string]interface{} `json:"parameters,omitempty"` // JSON Schema
	} `json:"function"`
}

// toolCall represents a tool call the model asks for (response side).
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // stringified JSON
	} `json:"function"`
}

// chatMsg is a message in the OpenAI dialect.
// Content is a RawMessage on purpose: the spec accepts a string, `null` (an
// assistant that only returns tool_calls) and an array of multimodal parts. A
// map[string]string handles neither of the last two and makes the parse of the
// whole body fail.
type chatMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []toolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// text extracts the text from content, tolerating a string, null and an array of parts.
func (m chatMsg) text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// withText returns a copy of the message with different text (used by the PII
// mask). When the original content was a multimodal array, it rebuilds an array
// with the text part replaced and every non-text part (images) preserved —
// collapsing to a bare string here used to silently drop any image on the same
// message the instant mask_pii was on, regardless of what the provider adapter
// did with ports.Message.Images afterwards (see decodeDataURLImage/images()).
func (m chatMsg) withText(s string) chatMsg {
	if len(m.Content) > 0 && m.Content[0] == '[' {
		var parts []json.RawMessage
		if json.Unmarshal(m.Content, &parts) == nil {
			rebuilt := make([]json.RawMessage, 0, len(parts))
			replaced := false
			for _, p := range parts {
				var head struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(p, &head) == nil && head.Type == "text" {
					if replaced {
						continue // a masked message carries exactly one text part
					}
					tb, _ := json.Marshal(struct {
						Type string `json:"type"`
						Text string `json:"text"`
					}{"text", s})
					rebuilt = append(rebuilt, tb)
					replaced = true
					continue
				}
				rebuilt = append(rebuilt, p) // image_url and any other part untouched
			}
			if !replaced {
				tb, _ := json.Marshal(struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}{"text", s})
				rebuilt = append([]json.RawMessage{tb}, rebuilt...)
			}
			if b, err := json.Marshal(rebuilt); err == nil {
				m.Content = b
				return m
			}
		}
	}
	b, _ := json.Marshal(s)
	m.Content = b
	return m
}

// toPortsMessages translates the handler's messages (chatMsg) into the outbound
// boundary (ports.Message). Raw carries the ORIGINAL content (string/null/multimodal
// array) so the adapter can rebuild the wire form without losing a byte; Text is the
// textual projection (what providers like bedrock/gemini use).
func toPortsMessages(msgs []chatMsg) []ports.Message {
	out := make([]ports.Message, len(msgs))
	for i, m := range msgs {
		pm := ports.Message{
			Role:       m.Role,
			Text:       m.text(),
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
			Images:     m.images(),
		}
		if len(m.Content) > 0 {
			pm.Raw = []byte(m.Content)
		}
		for _, tc := range m.ToolCalls {
			pm.ToolCalls = append(pm.ToolCalls, ports.ToolCall{
				ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
		out[i] = pm
	}
	return out
}

// toPortsTools translates the OpenAI dialect's tools (toolDef) into ports.ToolDef.
func toPortsTools(tools []toolDef) []ports.ToolDef {
	out := make([]ports.ToolDef, 0, len(tools))
	for _, t := range tools {
		out = append(out, ports.ToolDef{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return out
}

// fromPortsResult is the inverse of toPortsResult: it brings the adapter's
// ports.Result back into the result type the handler uses. tool_calls come back with
// type "function" (the only valid value in the OpenAI dialect).
func fromPortsResult(pr ports.Result) result {
	tc := make([]toolCall, 0, len(pr.ToolCalls))
	for _, t := range pr.ToolCalls {
		var c toolCall
		c.ID = t.ID
		c.Type = "function"
		c.Function.Name = t.Name
		c.Function.Arguments = t.Arguments
		tc = append(tc, c)
	}
	return result{
		text: pr.Text, tin: pr.InputTokens, tout: pr.OutputTokens,
		toolCalls: tc, stopReason: pr.StopReason,
		cacheRead: pr.CacheReadInputTokens, cacheWrite: pr.CacheWriteInputTokens,
		cacheConv: pr.CacheCounters,
	}
}

// callProviderFn is the provider's INJECTION SEAM (hexagonal-refactor).
//
// In production it points at callProvider, which now DISPATCHES to the adapters in
// internal/adapters/{bedrock,openaicompat,anthropic,google} (each implements
// ports.Provider). Tests replace it with a fake provider. Swapping this var does not
// change behavior: the default is the real function.
var callProviderFn = callProvider

// callProvider dispatches to the correct provider adapter, converting the boundary
// to and from the handler's types. Single-org model: org parameter removed.
func callProvider(ctx context.Context, r Route, msgs []chatMsg, tools []toolDef) (result, error) {
	in := ports.InvokeInput{Messages: toPortsMessages(msgs), Tools: toPortsTools(tools)}
	var p ports.Provider
	switch r.Provider {
	case "bedrock":
		// External ID for cross-account AssumeRole. Every customer's trust policy
		// must carry an ExternalID unique to them (the confused-deputy mitigation
		// AWS's cross-account docs call for) — a shared literal defeats that the
		// moment a second BYO customer exists, because it stops distinguishing
		// "assume MY role" from "assume ANYONE's role". When the route has a
		// role_arn but no explicit external_id (older routes, or the console left
		// it blank), derive the same "aiplat-<org>" value the console prefills and
		// the CloudFormation template asks the customer to set, instead of falling
		// back to a value every deployment/org would share.
		ext := r.ExternalID
		if r.RoleARN != "" && ext == "" {
			ext = "aiplat-" + deploymentOrg
		}
		p = &bedrock.Adapter{
			Pool:        bedrockPool,
			ModelID:     r.ProviderModelID,
			Route:       bedrock.Route{RoleARN: r.RoleARN, ExternalID: ext, Region: r.Region},
			CachePrefix: r.PromptCache,
		}
	case "openai_compatible":
		p = &openaicompat.Adapter{HTTP: httpc, BaseURL: r.BaseURL, ModelID: r.ProviderModelID, APIKey: getSecret(ctx, r.APIKeySecret)}
	case "anthropic":
		p = &anthropic.Adapter{HTTP: httpc, BaseURL: r.BaseURL, ModelID: r.ProviderModelID, APIKey: getSecret(ctx, r.APIKeySecret), CachePrefix: r.PromptCache}
	case "google", "gemini":
		p = &google.Adapter{HTTP: httpc, BaseURL: r.BaseURL, ModelID: r.ProviderModelID, APIKey: getSecret(ctx, r.APIKeySecret)}
	default:
		return result{}, fmt.Errorf("provider %s not supported", r.Provider)
	}
	pr, err := p.Invoke(ctx, in)
	if err != nil {
		return result{}, err
	}
	return fromPortsResult(pr), nil
}

// Routing defaults. They live here (the impure layer) and enter the domain as a
// value — the pure domain never reads the environment.
const defaultOutTokens = 512 // E[tokens_out] heuristic when there are no hints

// minHintSamples: below this the median is worse than an honest heuristic.
//
// It is CONFIG and not a constant because the right value depends on the
// environment's volume: with 20, a low-traffic environment would never use a hint at
// all and the path would go unvalidated. In production the default of 20 holds; in
// the PoC it is lowered.
func minHintSamples() int {
	if v, err := strconv.Atoi(os.Getenv("MIN_HINT_SAMPLES")); err == nil && v > 0 {
		return v
	}
	return 20
}

// realizedCost charges each token portion at the matching price tier, resolving the
// ambiguity of the provider's cache counting (Req 4.3/4.4).
// It also returns the savings attributable to the cache read.
func realizedCost(c *Config, model string, caps routing.Capabilities, res result, now time.Time) (cost, cacheSaved float64, status string) {
	tier, st := routing.SelectPrice(c.Pricing[model], now)
	if st == routing.PricingUnknown {
		// An unknown price does not invent cost: record zero and signal it.
		return 0, 0, st
	}
	split := routing.SplitInputTokens(res.tin, res.cacheRead, res.cacheWrite, caps.CacheTokensInclusive)
	return routing.RealizedCost(tier, caps.PerRequestFeeUSD, split, res.tout),
		routing.CacheSavings(tier, res.cacheRead), st
}

// upstreamOf returns the route's real DESTINATION (the "who served it"), derived from
// base_url. It distinguishes providers that share the same openai_compatible adapter
// (e.g. openrouter.ai vs api.groq.com vs api.together.xyz). No extra config.
func upstreamOf(r Route) string {
	if r.Provider == "bedrock" {
		return "bedrock"
	}
	bu := r.BaseURL
	if bu == "" {
		switch r.Provider {
		case "anthropic":
			return "api.anthropic.com"
		case "google", "gemini":
			return "generativelanguage.googleapis.com"
		}
		return r.Provider
	}
	if u, err := url.Parse(bu); err == nil && u.Host != "" {
		return u.Host
	}
	return bu
}

// Usage emission, structured logging and the failure taxonomy live in telemetry.go.
// Headers and HTTP response assembly in httpresp.go; SSE and streaming in stream.go.
// All three are protocol/output shell, not decision.

// identity is the team/app hierarchy resolved from the API key.
// The key is the ONLY source of truth for team/app (never the body).
// Single-org model: org field removed.
type identity struct {
	team string
	app  string
}

// authResolve validates the API key (Bearer) against the table and resolves team/app.
// It accepts "Authorization: Bearer <key>" or "x-aiplat-key" (used when
// the call arrives via CloudFront/OAC, which reserves the Authorization header for SigV4).
// authResolve returns (identity, ok, backendErr).
//   - backendErr != nil → PLATFORM failure (e.g. DynamoDB unavailable). It is NOT the
//     customer's fault → the handler answers 503 and it counts as our failure (SLI).
//   - ok == false with backendErr == nil → genuinely invalid/missing key → 401.
//
// authResolveFn is the AUTHENTICATION SEAM (hexagonal-refactor, task 1).
//
// Same pattern as callProviderFn: in production it points at authResolve (which talks
// to the API keys DynamoDB table). The characterization test replaces it with a fake
// resolution (fixed team/app) to drive handle() offline, since without
// APIKEYS_TABLE the real authResolve returns 401 and nothing else runs. Swapping this
// var does not change behavior: the default is the real function.
// Single-org model: org removed from resolution.
var authResolveFn = authResolve

func authResolve(ctx context.Context, headers map[string]string) (identity, bool, error) {
	// Extracting the key from the header is protocol shell (it stays here); resolving
	// it in the table belongs to the ddbkeys adapter (ports.KeyStore).
	auth := ""
	for k, v := range headers {
		switch strings.ToLower(k) {
		case "authorization":
			if auth == "" {
				auth = v
			}
		case "x-aiplat-key":
			auth = v
		}
	}
	key := strings.TrimSpace(auth)
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		key = strings.TrimSpace(auth[7:])
	}
	ident, ok, err := keyStore.Resolve(ctx, key)
	if err != nil || !ok {
		return identity{}, ok, err
	}
	return identity{team: ident.Team, app: ident.App}, true, nil
}

// The rate limit, budget and credit counters live in limits.go.

// decorateUsage used to add to the Usage_Record the fields the decision produced.
// Assembling the Usage_Record (decorateUsage/decorateSavings/decorateEscalation) now
// lives in the pure domain: internal/routing/record.go. The handler calls
// routing.DecorateUsage/DecorateSavings/DecorateEscalation.

// priceSourceOf returns the provenance of the price applied to the served model.
//
// Recording this per request is what makes it possible to answer, months later,
// whether that period's cost was computed from the provider's table or from the
// customer's negotiated price. Without the field, a customer who registers their
// contract today has no way to know which historical records already used the
// discount.
func priceSourceOf(c *Config, model string, now time.Time) string {
	tier, st := routing.SelectPrice(c.Pricing[model], now)
	if st == routing.PricingUnknown {
		return ""
	}
	return tier.SourceOf()
}

func estimateTokens(msgs []chatMsg) int {
	total := 0
	for _, m := range msgs {
		total += len(m.text())
	}
	return total / 4
}

type step struct {
	name string
	r    Route
}

func buildChain(c *Config, pol routing.Policy, chosen string, route Route, shape routing.RequestShape, now time.Time) []step {
	chain := []step{{chosen, route}}

	// The chain answers to the SAME eligibility authority as the decision.
	//
	// It did not before: Decide filtered by capability while this function filtered
	// only by allowed_models, so a request carrying tools could be retried on a
	// route with no tool use — the exact defect the eligibility layer exists to
	// prevent, one level down. `chosen` is exempt because Decide already cleared it.
	cands := candidates(c)
	eligible := routing.Eligible(cands, pol, shape)
	attemptable := func(m string) bool { return m == chosen || eligible[m] }

	// A bundle declares the attempt order by intent (same model first, then same
	// tier, then cheaper). It only reorders what is already allowed to serve.
	names := c.ModelOrder
	if bo := routing.BundleOrder(pol, cands); len(bo) > 0 {
		names = bo
	}

	// With AUTO-CHEAPEST the chain follows PRICE, not the manual order: it serves the
	// cheapest (chosen, already elected by Decide) and falls back to the NEXT cheapest
	// IN THE LIST (allowed + with a route). It does not ignore the list — it reorders
	// the attempts by expected cost. An unknown price goes last (there is no way to
	// rank it). This is what the customer asked for: a sequence from the cheapest to
	// the next one.
	if c.AutoCheapest {
		if len(names) == 0 {
			for m := range c.Routing {
				names = append(names, m)
			}
			sort.Strings(names) // determinism when no order is declared
		}
		eOut := shape.MaxOutputTokens
		if eOut <= 0 {
			eOut = defaultOutTokens
		}
		type scoredStep struct {
			name  string
			cost  routing.Money
			known bool
		}
		var cand []scoredStep
		seen := map[string]bool{chosen: true}
		for _, m := range names {
			if m == "" || seen[m] || !c.allowed(m) || !attemptable(m) {
				continue
			}
			fr, ok := c.Routing[m]
			if !ok {
				continue
			}
			seen[m] = true
			tier, st := routing.SelectPrice(c.Pricing[m], now)
			if st == routing.PricingUnknown {
				cand = append(cand, scoredStep{name: m, known: false})
				continue
			}
			cand = append(cand, scoredStep{name: m, known: true,
				cost: routing.ExpectedCost(tier, fr.Capabilities.PerRequestFeeUSD, shape.InputTokens, 0, eOut)})
		}
		sort.SliceStable(cand, func(i, j int) bool {
			if cand[i].known != cand[j].known {
				return cand[i].known // a known price comes before an unknown one
			}
			return cand[i].cost < cand[j].cost
		})
		for _, s := range cand {
			chain = append(chain, step{s.name, c.Routing[s.name]})
		}
		return chain
	}

	// The declared order (bundle, otherwise model_order) rules: everything that comes
	// AFTER the chosen model, and is allowed, is the fallback chain.
	if len(names) > 0 {
		after := false
		for _, m := range names {
			if m == chosen {
				after = true
				continue
			}
			if !after || m == "" || !c.allowed(m) || !attemptable(m) {
				continue
			}
			if fr, ok := c.Routing[m]; ok {
				chain = append(chain, step{m, fr})
			}
		}
		// If the chosen model is in the order, the order is the source of truth —
		// even when nothing follows it (end of the queue = no fallback).
		if after {
			return chain
		}
	}
	// With no order defined: fall back to the fallback declared on the route itself.
	for _, f := range route.Fallback {
		if !attemptable(f) {
			continue
		}
		if fr, ok := c.Routing[f]; ok {
			chain = append(chain, step{f, fr})
		}
	}
	return chain
}

// defaultModel resolves which model to use when the request carries no `model`:
// the first allowed one in the console's order; otherwise the explicit default_model.
func defaultModel(c *Config) string {
	for _, m := range c.ModelOrder {
		if m == "" || !c.allowed(m) {
			continue
		}
		if _, ok := c.Routing[m]; ok {
			return m
		}
	}
	if c.DefaultModel != "" && c.allowed(c.DefaultModel) {
		if _, ok := c.Routing[c.DefaultModel]; ok {
			return c.DefaultModel
		}
	}
	return ""
}

// applyGuardrails is the shell over chatMsg: it delegates the deterministic rules to
// the pure domain (routing.MaskPII/ContainsSecret/LooksLikeInjection). The
// loop/precedence logic is preserved byte for byte; only the patterns moved into the
// domain. Returns (possibly masked messages, block reason or "").
func applyGuardrails(g *GuardrailPolicy, msgs []chatMsg) ([]chatMsg, string) {
	if g == nil {
		// FAIL CLOSED. An absent policy used to mean "no guardrail", so a scope
		// with no explicit config forwarded the prompt verbatim to a third-party
		// provider. Absence of configuration must not be the least safe option:
		// the deterministic protections apply by default and have to be turned
		// OFF explicitly.
		g = defaultGuardrails()
	}
	for _, m := range msgs {
		c := m.text()
		if g.BlockSecrets && routing.ContainsSecret(c) {
			return msgs, "secret_detected"
		}
		if g.BlockInjection && routing.LooksLikeInjection(c) {
			return msgs, "prompt_injection"
		}
	}
	if g.MaskPII {
		out := make([]chatMsg, len(msgs))
		for i, m := range msgs {
			out[i] = m.withText(routing.MaskPII(m.text()))
		}
		return out, ""
	}
	return msgs, ""
}

// defaultGuardrails is the policy used when a scope declares none.
//
// Only the DETERMINISTIC rules are on by default:
//   - MaskPII and BlockSecrets are pattern matches with no judgement involved, so
//     enabling them cannot silently reject legitimate traffic — at worst an
//     e-mail in the prompt reaches the provider masked.
//   - BlockInjection stays OFF by default because it is a HEURISTIC: turning it
//     on by default would reject legitimate prompts (a false positive is a
//     rejected request, not a masked one). It is opt-in per scope, in the console.
func defaultGuardrails() *GuardrailPolicy {
	return &GuardrailPolicy{MaskPII: true, BlockSecrets: true}
}

// savings computes the saving vs. the requested model (ROI ledger).
//
// The saving is measured against what the customer REQUESTED, using the same token
// profile as the response that was actually served — that is the only honest
// comparison. Floor of zero: a negative saving never enters the ledger, otherwise the
// number that backs gain-share would be lying.
func savings(c *Config, requested, used string, res result, cost float64, now time.Time, swapClass string) (float64, string) {
	if used == requested || requested == "" {
		return 0, ""
	}
	tier, st := routing.SelectPrice(c.Pricing[requested], now)
	if st == routing.PricingUnknown {
		return 0, ""
	}
	caps := c.Routing[requested].Capabilities
	split := routing.SplitInputTokens(res.tin, res.cacheRead, res.cacheWrite, caps.CacheTokensInclusive)
	d := routing.RealizedCost(tier, caps.PerRequestFeeUSD, split, res.tout) - cost
	if d <= 0 {
		return 0, ""
	}
	// Same declared model served through a cheaper provider gets its own reason and
	// lands in the VERIFIED column of the ledger: there is no counterfactual
	// baseline to assume, because the model served IS the model requested. Checked
	// before auto_cheapest since the mechanism that found it is the same cost
	// ordering — what differs is that nothing about the response changed.
	if swapClass == routing.SwapSameModel {
		return d, routing.ReasonProviderArbitrage
	}
	if c.AutoCheapest {
		return d, "auto_cheapest"
	}
	return d, "fallback"
}

// verifiedPortion is the slice of the total saving that is OBSERVABLE rather than
// assumed — the number the ROI ledger may lean on for gain-share.
//
// Provider prompt cache always qualifies: the provider itself reported charging
// for fewer tokens. Provider arbitrage qualifies for the WHOLE amount, because the
// model served is the model requested — there is no counterfactual baseline to
// suppose, which is the same test that admits response cache.
//
// This function exists because leaving `cacheSaved` as the verified portion while
// ClassOf(reason) said "verified" produced a record that contradicted itself:
// class verified, money filed as counterfactual. The aggregation in Observability
// sums the columns, so the arbitrage saving would never have surfaced as proven.
func verifiedPortion(saved, cacheSaved float64, reason string) float64 {
	if reason == routing.ReasonProviderArbitrage {
		return saved
	}
	return cacheSaved
}

// canaryPick returns the candidate route that should serve this request as part of
// a declared experiment, or "" when no canary applies.
//
// Three guards, each earning its place:
//
//   - The candidate must pass the SAME eligibility as any other route. A canary is
//     an experiment about cost and latency, not a licence to send a tools request
//     to a route that cannot do tool use.
//   - Eligibility here includes the feature's swap ceiling, so a customer who
//     declared same_model_only never gets a different model through the canary.
//     Two declarations conflict there and the guarantee has to win over the
//     experiment — otherwise the strongest promise of the feature would have a hole
//     in it shaped like a config field.
//   - An ineligible or unknown candidate falls back to the reference silently:
//     refusing traffic because an experiment was misconfigured would be absurd.
func canaryPick(c *Config, pol routing.Policy, feature string, shape routing.RequestShape, requestID, reference string) string {
	fp, ok := c.FeaturePolicy[feature]
	if !ok || fp.Canary == nil {
		return ""
	}
	cand := fp.Canary.Route
	if cand == "" || cand == reference {
		return ""
	}
	if _, ok := c.Routing[cand]; !ok || !c.allowed(cand) {
		return ""
	}
	if !routing.InCanary(*fp.Canary, requestID) {
		return ""
	}
	if !routing.Eligible(candidates(c), pol, shape)[cand] {
		return ""
	}
	return cand
}

// decorateCanary marks a record as sampled by an experiment. Present only when it
// happened: every request carrying `canary:false` would double the field count of
// the log for no information.
func decorateCanary(m map[string]interface{}, canaryRoute string) {
	if canaryRoute == "" {
		return
	}
	m["canary"] = true
	m["canary_route"] = canaryRoute
}

// servedSwap classifies what actually reached the provider — not what the decision
// picked. The two diverge when the decided route failed and the fallback loop moved
// on, and the ledger has to describe the response the customer RECEIVED.
//
// Returns the class and the declared identity of the served route ("" when the
// customer declared none; identity is never inferred).
func servedSwap(c *Config, requested, used string) (string, string) {
	id := routing.BuildIdentity(candidates(c))
	servedModelID := id.ModelIDOf(used)
	if requested == "" || used == requested {
		return routing.SwapNone, servedModelID
	}
	req := routing.Candidate{Model: requested, Caps: c.Routing[requested].Capabilities}
	srv := routing.Candidate{Model: used, Caps: c.Routing[used].Capabilities}
	return id.SwapClassOf(req, srv), servedModelID
}

// decorateSwap writes the substitution dimension into a map that goes out — the
// Usage_Record or the `aiplat` block of the response.
//
// swap_class is always present, including as the empty "no swap": it is a
// first-class dimension of the ledger (like savings_class), and a reader must be
// able to tell "served as requested" from "field missing from an old record".
// served_model_id, in contrast, only appears when DECLARED — an empty string there
// would read as an identity that exists and is blank.
func decorateSwap(m map[string]interface{}, class, servedModelID string) {
	m["swap_class"] = class
	if servedModelID != "" {
		m["served_model_id"] = servedModelID
	}
}

// The local `semQueryText` function used to live here and was REMOVED on purpose.
//
// It projected ALL messages (including the system prompt) into the text to be
// vectorized — the defect that made a question about football receive the answer
// about banking (0.96 measured similarity, against 0.29 using only the question).
// The handler calls `routing.SemQueryText`, which projects only the user turns.
//
// It was dead code, but dead in the worst way: a faithful implementation of the
// defect, with a plausible name, one call site away from being revived by someone
// "fixing" an import. Deleting it is the only way not to leave that trap in the file.

// indexSemantic appends (cacheKey → question vector) to the deployment's semantic index,
// with dedup by cacheKey and a FIFO cap (semIndexCap). It reuses the vector already
// computed on the read — no extra embedding call on the write.
// Best-effort: any failure is silent (the exact cache keeps working).
// semCtx is the fingerprint of the context (system prompt + model) that partitions the
// index: without it, different personas and models would share a response.
// Single-org model: org parameter removed, index is deployment-level.
func indexSemantic(ctx context.Context, cacheKey string, vec []float32, semCtx, semNum string, ttlSeconds int) {
	// An empty semCtx means we do not know how to partition — do not index, otherwise
	// the entry would be born unusable (which is exactly the state of the entries
	// written before the fix).
	if semStore == nil || len(vec) == 0 || semCtx == "" {
		return
	}
	q, scale := routing.QuantizeVec(vec)
	raw := make([]byte, len(q))
	for i, x := range q {
		raw[i] = byte(x)
	}
	entry := ddbcache.SemEntry{CacheKey: cacheKey, Q: base64.StdEncoding.EncodeToString(raw), Scale: scale, Ctx: semCtx, Num: semNum}
	out := make([]ddbcache.SemEntry, 0, semIndexCap)
	out = append(out, entry)
	for _, e := range semStore.GetSemIndex(ctx, "default") {
		if e.CacheKey == cacheKey {
			continue
		}
		out = append(out, e)
		if len(out) >= semIndexCap {
			break
		}
	}
	semStore.PutSemIndex(ctx, "default", out, ttlSeconds)
}

// hasImage detects multimodal content for the eligibility filter (Req 1.3).
// It looks at the SHAPE of content (array of parts with type image_*), never at the
// content itself.
func hasImage(msgs []chatMsg) bool {
	for _, m := range msgs {
		if len(m.Content) == 0 || m.Content[0] != '[' {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if strings.HasPrefix(p.Type, "image") {
				return true
			}
		}
	}
	return false
}

// dataURLImageFormat/dataURLImageBytes split a data URL of the shape
// "data:image/png;base64,AAAA..." into the bare format ("png") and the
// base64-decoded bytes. ok is false for anything else (a remote http(s) URL, a
// malformed data URL, or bad base64) — callers drop the part rather than send a
// provider bytes it did not ask for.
func decodeDataURLImage(url string) (format string, data []byte, ok bool) {
	const prefix = "data:image/"
	if !strings.HasPrefix(url, prefix) {
		return "", nil, false
	}
	rest := url[len(prefix):]
	semi := strings.IndexByte(rest, ';')
	comma := strings.IndexByte(rest, ',')
	if semi < 0 || comma < 0 || comma < semi {
		return "", nil, false
	}
	format = rest[:semi]
	encoding := rest[semi+1 : comma]
	if encoding != "base64" {
		return "", nil, false
	}
	data, err := base64.StdEncoding.DecodeString(rest[comma+1:])
	if err != nil {
		return "", nil, false
	}
	return format, data, true
}

// extractImages decodes the image_url parts of a multimodal message's content
// into ports.ImagePart (see the type's doc for why this is centralized here
// instead of in each provider adapter). Text-only or malformed content yields nil,
// same as hasImage above — a message either has decodable images or it does not.
func (m chatMsg) images() []ports.ImagePart {
	if len(m.Content) == 0 || m.Content[0] != '[' {
		return nil
	}
	var parts []struct {
		Type     string `json:"type"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return nil
	}
	var out []ports.ImagePart
	for _, p := range parts {
		if !strings.HasPrefix(p.Type, "image") {
			continue
		}
		format, data, ok := decodeDataURLImage(p.ImageURL.URL)
		if !ok {
			continue
		}
		out = append(out, ports.ImagePart{Format: format, Bytes: data})
	}
	return out
}

// discardSummary summarizes the discards for the Usage_Record's `detail` field.
// Metadata only: model names and reasons, never content.
func discardSummary(d routing.Decision) string {
	if len(d.Discards) == 0 {
		return ""
	}
	parts := make([]string, 0, len(d.Discards))
	for _, x := range d.Discards {
		parts = append(parts, x.Model+":"+x.Reason)
	}
	return strings.Join(parts, ",")
}

// logDecision makes the decision AUDITABLE (Req 8.2): without it, the customer paying
// gain-share on proven savings has no way to check the number. It records the chosen
// model, the requested one, the expected cost, the reason for each discard and the
// origin of E[tokens_out] — never the prompt nor the response.
func logDecision(reqID string, id identity, feature, requested string, d routing.Decision, shape routing.RequestShape, lat int) {
	f := map[string]interface{}{
		"lvl": "info", "evt": "routing_decision", "request_id": reqID,
		"team": id.team, "app": id.app, "feature": feature,
		"requested_model": requested, "model": d.Model,
		"expected_cost_usd": d.ExpectedCostUSD, "cash_cost_usd": d.CashCostUSD,
		"requested_cost_usd": d.RequestedCostUSD,
		"pricing_status":     d.PricingStatus, "out_tokens_source": d.OutTokensSource,
		"paid_from": d.PaidFrom, "economy_mode": d.EconomyMode,
		"availability_degraded": d.AvailabilityDegraded,
		"input_tokens":          shape.InputTokens, "has_tools": shape.HasTools,
		"has_image": shape.HasImage, "latency_ms": lat,
	}
	if len(d.Discards) > 0 {
		ds := make([]map[string]string, 0, len(d.Discards))
		for _, x := range d.Discards {
			ds = append(ds, map[string]string{"model": x.Model, "reason": x.Reason})
		}
		f["discards"] = ds
	}
	logJSON(f)
}

// escalationEnabled: opt-in per feature. The default follows economy mode — whoever
// accepted worse output is the one who needs the safety net.
func escalationEnabled(c *Config, feature string) bool {
	fp, ok := c.FeaturePolicy[feature]
	if !ok {
		return false
	}
	if fp.Escalate != nil {
		return *fp.Escalate
	}
	return fp.EconomyMode
}

// routerProvider adapts the provider call to the ports.Provider port.
//
// It is the transition wrapper described in the design: it satisfies the interface by
// delegating to callProvider, without changing a single line of the four existing
// adapters. It is also the PRODUCTION implementation of the port — what lets the
// escalation in the pure domain be exercised with a fake provider under test and with
// the real one here. Single-org model: org field removed.
type routerProvider struct {
	cfg   *Config
	msgs  []chatMsg
	tools []toolDef

	// raw keeps the raw result per model, because only it carries the cache counters
	// in the shape realizedCost() needs.
	raw map[string]result
}

func (p *routerProvider) Invoke(ctx context.Context, in ports.InvokeInput) (ports.Result, error) {
	r, ok := p.cfg.Routing[in.Model]
	if !ok {
		return ports.Result{}, fmt.Errorf("unknown model: %s", in.Model)
	}
	res, err := callProviderFn(ctx, r, p.msgs, p.tools)
	if err != nil {
		return ports.Result{}, err
	}
	if p.raw == nil {
		p.raw = map[string]result{}
	}
	p.raw[in.Model] = res
	return toPortsResult(res), nil
}

var _ ports.Provider = (*routerProvider)(nil) // compile-time assertion

func toPortsResult(r result) ports.Result {
	tc := make([]ports.ToolCall, 0, len(r.toolCalls))
	for _, t := range r.toolCalls {
		tc = append(tc, ports.ToolCall{ID: t.ID, Name: t.Function.Name, Arguments: t.Function.Arguments})
	}
	return ports.Result{
		Text: r.text, InputTokens: r.tin, OutputTokens: r.tout,
		CacheReadInputTokens: r.cacheRead, CacheWriteInputTokens: r.cacheWrite,
		CacheCounters: r.cacheConv,
		ToolCalls:     tc, StopReason: r.stopReason,
	}
}

// escalation is the escalation result translated back into the handler's terms.
type escalation struct {
	res       result
	model     string
	escalated bool
	reason    string
	outcome   string
	attempts  int
	totalCost float64
}

// maybeEscalate validates the response and, if structurally broken, retries ONCE on a
// higher-tier model — delegating the logic to the pure domain.
//
// The first call was already made by the fallback loop, so we inject a provider that
// returns that result on the first invocation and calls the real provider on the
// following ones. That way we do not pay for the first call twice.
// Single-org model: org parameter removed.
func maybeEscalate(ctx context.Context, c *Config, s step,
	msgs []chatMsg, tools []toolDef, maxOut int, first result,
	shape routing.RequestShape, now time.Time,
) escalation {
	prov := &primedProvider{
		primed: map[string]result{s.name: first},
		inner:  &routerProvider{cfg: c, msgs: msgs, tools: tools},
	}

	costFn := func(model string, pr ports.Result) routing.Money {
		raw, ok := prov.rawOf(model)
		if !ok {
			// Without the raw result there are no cache counters: fall back to the
			// simple projection.
			raw = result{tin: pr.InputTokens, tout: pr.OutputTokens}
		}
		cost, _, _ := realizedCost(c, model, c.Routing[model].Capabilities, raw, now)
		return cost
	}

	cands := candidates(c)
	costOf := func(cd routing.Candidate) (routing.Money, bool) {
		tier, st := routing.SelectPrice(cd.Prices, now)
		if st == routing.PricingUnknown {
			return 0, false
		}
		return routing.ExpectedCost(tier, cd.Caps.PerRequestFeeUSD, shape.InputTokens, 0, defaultOutTokens), true
	}

	in := ports.InvokeInput{Model: s.name, MaxOutputTokens: maxOut}
	out, err := routing.Escalate(ctx, prov, in, shape, costFn,
		routing.NextTierFrom(cands, s.name, costOf), true, false)
	if err != nil {
		return escalation{res: first, model: s.name, attempts: 1,
			totalCost: costFn(s.name, toPortsResult(first))}
	}

	final := out.Final()
	raw, ok := prov.rawOf(final.Model)
	if !ok {
		raw = first
	}
	return escalation{
		res: raw, model: final.Model, escalated: out.Escalated,
		reason: out.Reason, outcome: out.Outcome,
		attempts: len(out.Attempts), totalCost: out.TotalCost,
	}
}

// primedProvider returns an ALREADY OBTAINED result on the first invocation of a model
// and delegates to the real provider on the others. It avoids repeating the call the
// fallback loop already made — repeating it would spend the customer's money on a
// decision of ours.
type primedProvider struct {
	primed map[string]result
	inner  *routerProvider
	used   map[string]bool
}

func (p *primedProvider) Invoke(ctx context.Context, in ports.InvokeInput) (ports.Result, error) {
	if p.used == nil {
		p.used = map[string]bool{}
	}
	if r, ok := p.primed[in.Model]; ok && !p.used[in.Model] {
		p.used[in.Model] = true
		return toPortsResult(r), nil
	}
	return p.inner.Invoke(ctx, in)
}

// rawOf returns a model's raw result (with cache counters).
func (p *primedProvider) rawOf(model string) (result, bool) {
	if r, ok := p.inner.raw[model]; ok {
		return r, true
	}
	r, ok := p.primed[model]
	return r, ok
}

var _ ports.Provider = (*primedProvider)(nil)

// handle is the Core's decision function, on the neutral httpapi.Request boundary.
// No Lambda type appears here — translating the API Gateway event happens in
// internal/awslambda (Requirement 1.1, 1.4).
func handle(ctx context.Context, req httpapi.Request) (apiResp, error) {
	start := time.Now()
	// Captured once and threaded through every response: CORS is decided by the
	// allowlist in httpresp.go (deny by default, no wildcard), so every exit path
	// needs the request's own origin to be able to echo it.
	reqOrigin := originOf(req.Headers)
	if req.Method == "OPTIONS" {
		return sresp(204, corsHeaders(reqOrigin), "")
	}

	id, ok, aerr := authResolveFn(ctx, req.Headers)
	if aerr != nil {
		// PLATFORM: the auth backend (DynamoDB) is unavailable — NOT the customer's
		// fault. 503, and it counts as our failure. With no org, it goes only to
		// CloudWatch.
		logJSON(map[string]interface{}{
			"lvl": "error", "evt": "gateway_request", "status": "error",
			"reason": "auth_backend_error", "category": catPlatform, "sli_eligible": true,
			"detail": aerr.Error(), "latency_ms": int(time.Since(start).Milliseconds()),
		})
		fmt.Println("AIPLAT_SLI_FAIL reason=auth_backend_error category=" + catPlatform)
		return jerr(reqOrigin, 503, "auth backend temporarily unavailable")
	}
	if !ok {
		return jerr(reqOrigin, 401, "unauthorized")
	}
	ident := id // stable copy (the name `id` is shadowed inside the streaming block)
	app, team := id.app, id.team

	// The base64 decoding (IsBase64Encoded) already happened in the inbound adapter
	// (internal/awslambda); here the body already arrives as text.
	raw := req.Body
	var body struct {
		Model    string    `json:"model"`
		Messages []chatMsg `json:"messages"`
		Tools    []toolDef `json:"tools,omitempty"` // Function calling / tool use
		// Pointers: they distinguish "not provided" from "zero" for the cache key
		// (a deterministic temperature=0 ≠ absent; same for max_tokens).
		MaxTokens   *int     `json:"max_tokens,omitempty"`
		Temperature *float64 `json:"temperature,omitempty"`
		NoCache     bool     `json:"no_cache"`
		Feature     string   `json:"feature"`
		Stream      bool     `json:"stream"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		emitFailure(ctx, req.RequestID, ident, "", "", "", "blocked", "invalid_body", "", int(time.Since(start).Milliseconds()))
		return jerr(reqOrigin, 400, "invalid JSON body")
	}

	feature := body.Feature
	if feature == "" {
		for k, v := range req.Headers {
			if strings.ToLower(k) == "x-aiplat-feature" {
				feature = v
			}
		}
	}

	// Effective config for the scope (global → team → app), with fallback to defaults.
	// Single-org model: org removed from hierarchy.
	c := loadConfig(ctx, team, app)

	// Account kill-switch: the operator (backoffice) suspends by writing status into
	// the org's config. The gateway refuses immediately — that is what makes the
	// suspension real.
	if c.Status == "suspended" {
		emitFailure(ctx, req.RequestID, ident, feature, "", body.Model, "blocked", "account_suspended", "", int(time.Since(start).Milliseconds()))
		return sresp(403, jsonHeaders(reqOrigin),
			`{"error":{"message":"account suspended; contact the platform operator","type":"account_suspended","code":"account_suspended"}}`)
	}

	requested := body.Model
	// Request with no `model`: use the org's default (1st in the console's order).
	// The default_model field used to be written by the console and ignored here.
	if requested == "" {
		requested = defaultModel(c)
		if requested == "" {
			emitFailure(ctx, req.RequestID, ident, feature, "", "", "blocked", "unknown_model", "no model and no default", int(time.Since(start).Milliseconds()))
			return jerr(reqOrigin, 400, "no model specified and this org has no default model configured")
		}
	}
	// Allowed models policy (inherited from the most specific scope).
	if !c.allowed(requested) {
		emitFailure(ctx, req.RequestID, ident, feature, "", requested, "blocked", "model_not_allowed", "", int(time.Since(start).Milliseconds()))
		return jerr(reqOrigin, 403, "model not allowed for this scope: "+requested)
	}

	// --- Content guardrails (policy per scope) ---
	// Blocks secret/injection; masks PII before leaving for the provider.
	if msgs, reason := applyGuardrails(c.Guardrails, body.Messages); reason != "" {
		emitFailure(ctx, req.RequestID, ident, feature, "", requested, "blocked", reason, "", int(time.Since(start).Milliseconds()))
		return sresp(400, jsonHeaders(reqOrigin),
			`{"error":{"message":"blocked by content guardrail: `+reason+`","type":"policy_violation","code":"`+reason+`"}}`)
	} else {
		body.Messages = msgs
	}
	// no_store: zero retention → never reads nor writes the response cache.
	noStore := body.NoCache || (c.Guardrails != nil && c.Guardrails.NoStore)

	// --- Rate limit (in the scope that DEFINED the limit) ---
	if !checkRate(ctx, c.limitsScope, c.Limits) {
		emitFailure(ctx, req.RequestID, ident, feature, "", requested, "blocked", "rate_limit_exceeded", "", int(time.Since(start).Milliseconds()))
		return sresp(429, allowCORS(map[string]string{"content-type": "application/json",
			"retry-after": "60"}, reqOrigin),
			`{"error":{"message":"rate limit exceeded for scope `+c.limitsScope+`","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	}

	// --- Monthly budget: alert | degrade | block ---
	budgetState := ""
	if lim := c.Budget.Limit(); lim > 0 {
		spent := readSpend(ctx, c.budgetScope)
		if spent >= lim {
			switch c.Budget.Action {
			case "block":
				emitFailure(ctx, req.RequestID, ident, feature, "", requested, "blocked", "budget_exceeded", "", int(time.Since(start).Milliseconds()))
				return sresp(429, jsonHeaders(reqOrigin),
					`{"error":{"message":"monthly budget exceeded for scope `+c.budgetScope+`","type":"insufficient_quota","code":"budget_exceeded"}}`)
			case "degrade":
				budgetState = "exceeded_degraded"
			default:
				budgetState = "exceeded_alert"
			}
		}
	}

	// --- Routing decision (pure domain) --------------------------------------
	// Three layers: eligibility (hard) → availability → expected cost.
	// Eligibility ALWAYS applies, including with auto_cheapest off: a model's
	// capability does not depend on cost optimization being active. That was exactly
	// the hole that sent tool use to a model without tool use.
	now := time.Now().UTC()
	pol := policyFor(c, feature)
	// An exceeded budget with action "degrade" forces cost optimization on.
	if budgetState == "exceeded_degraded" {
		pol.AutoCheapest = true
	}
	maxTok := 0
	if body.MaxTokens != nil {
		maxTok = *body.MaxTokens
	}
	shape := routing.RequestShape{
		InputTokens:     estimateTokens(body.Messages),
		MaxOutputTokens: maxTok,
		HasTools:        len(body.Tools) > 0,
		HasImage:        hasImage(body.Messages),
		Feature:         feature,
		RequestedModel:  requested,
	}
	// Hints: historical E[tokens_out] and the unavailability signal, read as a
	// contract. nil here is the normal path (new org, publisher down) and the domain
	// falls back to the heuristic — it never fails because of this.
	// Single-org model: Routing_Hints and credit counters are read under the
	// deployment-level "default" partition — the same value the semantic index
	// (indexSemantic) already uses. Observability still partitions the Cost_Store
	// by org because it aggregates across the fleet of single-client deployments
	// it may serve; this gateway has exactly one.
	var hints *routing.Hints
	if hintsReader != nil {
		hints = hintsReader.Hints(ctx, "default", feature, now)
		if hints == nil {
			logJSON(map[string]interface{}{
				"lvl": "info", "evt": "hints_unavailable", "feature": feature,
			})
		}
	}
	dec, derr := routing.Decide(candidates(c), pol, hints, creditState(ctx, c, now), shape, now)
	if derr != nil {
		lat := int(time.Since(start).Milliseconds())
		logDecision(req.RequestID, ident, feature, requested, dec, shape, lat)
		// Declared failure: capable routes existed, but every one of them would have
		// required a substitution the feature's swap ceiling forbids. Answered BEFORE
		// no_eligible_model and with its own code, because the customer's next action
		// is the opposite: here they loosen the policy (or fix the identity
		// declaration), there they fix the catalog.
		if errors.Is(derr, routing.ErrSwapNotAllowed) {
			emitFailure(ctx, req.RequestID, ident, feature, "", requested, "blocked", "swap_not_allowed", discardSummary(dec), lat)
			return sresp(400, jsonHeaders(reqOrigin),
				`{"error":{"message":"no route allowed by the swap policy of this feature","type":"invalid_request_error","code":"swap_not_allowed"}}`)
		}
		if errors.Is(derr, routing.ErrNoEligibleModel) {
			emitFailure(ctx, req.RequestID, ident, feature, "", requested, "blocked", "no_eligible_model", discardSummary(dec), lat)
			return sresp(400, jsonHeaders(reqOrigin),
				`{"error":{"message":"no model can serve this request in the current scope","type":"invalid_request_error","code":"no_eligible_model"}}`)
		}
		if errors.Is(derr, routing.ErrUnknownModel) {
			emitFailure(ctx, req.RequestID, ident, feature, "", requested, "blocked", "unknown_model", "", lat)
			return jerr(reqOrigin, 400, "unknown model: "+requested)
		}
		emitFailure(ctx, req.RequestID, ident, feature, "", requested, "blocked", "unknown_model", fmt.Sprint(derr), lat)
		return jerr(reqOrigin, 400, "unknown model: "+requested)
	}
	chosen := dec.Model
	// Canary: a declared slice of this feature's traffic serves on the candidate
	// route instead, so the comparison is made on the customer's real prompts. The
	// substitution is recorded, so the ledger never presents experiment traffic as
	// if it had been the customer's own choice.
	canaryRoute := canaryPick(c, pol, feature, shape, req.RequestID, chosen)
	if canaryRoute != "" {
		chosen = canaryRoute
	}
	route, ok := c.Routing[chosen]
	if !ok {
		emitFailure(ctx, req.RequestID, ident, feature, "", chosen, "blocked", "unknown_model", "", int(time.Since(start).Milliseconds()))
		return jerr(reqOrigin, 400, "unknown model: "+chosen)
	}
	logDecision(req.RequestID, ident, feature, requested, dec, shape, int(time.Since(start).Milliseconds()))
	if dec.PricingStatus == routing.PricingUnknown {
		// Do not invent cost: signal so the operator fixes the registration.
		logJSON(map[string]interface{}{
			"lvl": "warn", "evt": "pricing_missing", "request_id": req.RequestID,
			"model": chosen, "scope": c.budgetScope,
		})
	}
	// Cache key in the pure domain: it includes tools/temperature/max_tokens (defect
	// fix) and honors the mode (exact|canonical) inherited from the config. The org is
	// the first thing hashed — the cache never crosses orgs.
	keyMode := routing.NormalizeKeyMode(c.CacheKeyMode)
	ck := routing.CacheKey(routing.KeyInput{
		Org: "default", Model: chosen,
		Messages:    toPortsMessages(body.Messages),
		Tools:       toPortsTools(body.Tools),
		Temperature: body.Temperature,
		MaxTokens:   body.MaxTokens,
	}, keyMode)

	// --- Cache hit ---
	if cacheStore.Enabled() && !noStore {
		if respJSON, savedCache, hit := cacheStore.Get(ctx, ck); hit {
			lat := int(time.Since(start).Milliseconds())
			var cached map[string]interface{}
			json.Unmarshal([]byte(respJSON), &cached)
			cached["aiplat"] = map[string]interface{}{"team": team, "app_tag": app, "feature": feature, "model": chosen, "provider": route.Provider, "estimated_cost_usd": 0, "saved_usd": savedCache, "savings_reason": "cache", "cache_hit": true, "latency_ms": lat, "auto_cheapest": c.AutoCheapest, "requested_model": requested}
			cacheRec := map[string]interface{}{"request_id": req.RequestID, "cache_key": ck, "team": team, "app_tag": app, "feature": feature, "provider": route.Provider, "upstream": upstreamOf(route), "model": chosen, "tokens_in": 0, "tokens_out": 0, "estimated_cost_usd": 0, "saved_usd": savedCache, "savings_reason": "cache", "latency_ms": lat, "cache_hit": true, "status": "success", "category": "ok", "sli_eligible": true, "ts": time.Now().UTC().Format(time.RFC3339)}
			// The swap counts on a cache hit too: the response came from the DECIDED
			// route, which may not be the requested one. Omitting the class here would
			// leave a hole in the trail exactly on the cheapest requests — and a
			// customer auditing quality would see "served as requested" where a
			// substitution took place.
			cacheSwap, cacheServedID := servedSwap(c, requested, chosen)
			decorateSwap(cacheRec, cacheSwap, cacheServedID)
			decorateSwap(cached["aiplat"].(map[string]interface{}), cacheSwap, cacheServedID)
			decorateCanary(cacheRec, canaryRoute)
			decorateCanary(cached["aiplat"].(map[string]interface{}), canaryRoute)
			// A cache hit is 100% VERIFIED savings: same model, identical response, no
			// call to the provider. There is no assumed baseline.
			routing.DecorateSavings(cacheRec, savedCache, savedCache, "cache")
			emitUsageFn(ctx, cacheRec)
			if body.Stream {
				text := ""
				if ch, ok := cached["choices"].([]interface{}); ok && len(ch) > 0 {
					if m, ok := ch[0].(map[string]interface{}); ok {
						if msg, ok := m["message"].(map[string]interface{}); ok {
							if s, ok := msg["content"].(string); ok {
								text = s
							}
						}
					}
				}
				var sb bytes.Buffer
				pseudoStream(&sb, "chatcmpl-"+ck[:12], chosen, text)
				return sresp(200, sseHeaders(reqOrigin), sb.String())
			}
			return jbody(reqOrigin, 200, cached)
		}
	}

	// --- Semantic cache (opt-in) ---
	// After the exact/canonical miss: vectorize the question (Titan) and search the
	// org's index for the answer to a SEMANTICALLY close question. An approximate
	// match (it admits false positives) → savings marked as semantic_cache
	// (counterfactual, not verified). Everything is degradable: any failure here just
	// continues on to the provider. The vector computed here is reused on the write
	// (no duplicated embed).
	var semQueryVec []float32
	var semQueryCtx, semQueryNum string
	if c.SemanticCache && !noStore && embedder != nil && semStore != nil && cacheStore.Enabled() {
		// Only the USER turns are vectorized. Including the system prompt made the
		// embedding represent the PROMPT and not the question: in an app with a
		// ~930-char prompt and a ~37-char question, two unrelated questions measured
		// 0.96 similarity (threshold 0.92) and one received the other's answer.
		// The prompt is not ignored — it PARTITIONS the index via semCtx.
		if qtext := routing.SemQueryText(toPortsMessages(body.Messages)); qtext != "" {
			if qvec, eerr := embedder.Embed(ctx, qtext); eerr == nil && len(qvec) > 0 {
				semQueryVec = qvec
				semQueryCtx = routing.SemContextKey(toPortsMessages(body.Messages), chosen)
				// Numeric guard: an embedding does not tell 60 from 600 apart
				// (measured: 0.93, above the threshold). Different numbers ⇒ never a
				// match.
				semQueryNum = routing.NumFingerprint(qtext)
				entries := semStore.GetSemIndex(ctx, "default")
				cands := make([]routing.SemCandidate, 0, len(entries))
				for _, e := range entries {
					raw, derr := base64.StdEncoding.DecodeString(e.Q)
					if derr != nil {
						continue
					}
					i8 := make([]int8, len(raw))
					for i, bb := range raw {
						i8[i] = int8(bb)
					}
					cands = append(cands, routing.SemCandidate{CacheKey: e.CacheKey, Vec: routing.DequantizeVec(i8, e.Scale), Ctx: e.Ctx, Num: e.Num})
				}
				if mt, ok := routing.BestSemanticMatch(qvec, cands, c.SemanticThreshold, semQueryCtx, semQueryNum); ok {
					if respJSON, savedCache, hit := cacheStore.Get(ctx, mt.CacheKey); hit {
						lat := int(time.Since(start).Milliseconds())
						var cached map[string]interface{}
						json.Unmarshal([]byte(respJSON), &cached)
						cached["aiplat"] = map[string]interface{}{"team": team, "app_tag": app, "feature": feature, "model": chosen, "provider": route.Provider, "estimated_cost_usd": 0, "saved_usd": savedCache, "savings_reason": routing.ReasonSemanticCache, "savings_class": routing.ClassOf(routing.ReasonSemanticCache), "cache_hit": true, "semantic_score": mt.Score, "latency_ms": lat, "auto_cheapest": c.AutoCheapest, "requested_model": requested}
						cacheRec := map[string]interface{}{"request_id": req.RequestID, "cache_key": ck, "team": team, "app_tag": app, "feature": feature, "provider": route.Provider, "upstream": upstreamOf(route), "model": chosen, "tokens_in": 0, "tokens_out": 0, "estimated_cost_usd": 0, "saved_usd": savedCache, "savings_reason": routing.ReasonSemanticCache, "latency_ms": lat, "cache_hit": true, "status": "success", "category": "ok", "sli_eligible": true, "ts": time.Now().UTC().Format(time.RFC3339)}
						semSwap, semServedID := servedSwap(c, requested, chosen)
						decorateSwap(cacheRec, semSwap, semServedID)
						decorateSwap(cached["aiplat"].(map[string]interface{}), semSwap, semServedID)
						decorateCanary(cacheRec, canaryRoute)
						decorateCanary(cached["aiplat"].(map[string]interface{}), canaryRoute)
						// Semantic is NOT verified savings (approximate response):
						// verifiedPortion=0 → everything is filed as counterfactual.
						routing.DecorateSavings(cacheRec, savedCache, 0, routing.ReasonSemanticCache)
						emitUsageFn(ctx, cacheRec)
						if body.Stream {
							text := ""
							if chs, ok := cached["choices"].([]interface{}); ok && len(chs) > 0 {
								if m0, ok := chs[0].(map[string]interface{}); ok {
									if msg, ok := m0["message"].(map[string]interface{}); ok {
										if sc, ok := msg["content"].(string); ok {
											text = sc
										}
									}
								}
							}
							var sb bytes.Buffer
							pseudoStream(&sb, "chatcmpl-"+ck[:12], chosen, text)
							return sresp(200, sseHeaders(reqOrigin), sb.String())
						}
						return jbody(reqOrigin, 200, cached)
					}
				}
			}
		}
	}

	chain := buildChain(c, pol, chosen, route, shape, now)

	// --- Streaming (SSE) ---
	// Note: API Gateway buffers the response, so the SSE frames are assembled and sent
	// at the end (a valid format for the SDK; not incremental token by token).
	if body.Stream {
		var sb bytes.Buffer
		id := "chatcmpl-" + ck[:12]
		var content string
		var tin, tout int
		usedName, usedProvider, usedUpstream := chosen, "", ""
		var perr error
		okStream := false
		for _, s := range chain {
			usedName, usedProvider, usedUpstream = s.name, s.r.Provider, upstreamOf(s.r)
			if s.r.Provider == "openai_compatible" {
				ct, ti, to, e := streamOpenAICompat(ctx, &sb, s.r.BaseURL, s.r.ProviderModelID, body.Messages, getSecret(ctx, s.r.APIKeySecret))
				if e == nil {
					content, tin, tout, okStream = ct, ti, to, true
					break
				}
				perr = e
				continue
			}
			res, e := callProviderFn(ctx, s.r, body.Messages, body.Tools)
			if e == nil {
				content, tin, tout, okStream = res.text, res.tin, res.tout, true
				pseudoStream(&sb, id, s.name, res.text)
				break
			}
			perr = e
		}
		if !okStream {
			emitFailure(ctx, req.RequestID, ident, feature, usedProvider, usedName, "error", classifyProviderErr(perr), fmt.Sprint(perr), int(time.Since(start).Milliseconds()))
			return jerr(reqOrigin, 502, "all providers failed: "+fmt.Sprint(perr))
		}
		if tout == 0 {
			tout = len(content) / 4
		}
		if tin == 0 {
			tin = estimateTokens(body.Messages)
		}
		lat := int(time.Since(start).Milliseconds())
		usedCaps := c.Routing[usedName].Capabilities
		streamRes := result{tin: tin, tout: tout}
		cost, cacheSaved, pricingStatus := realizedCost(c, usedName, usedCaps, streamRes, now)
		swapClass, servedModelID := servedSwap(c, requested, usedName)
		saved, reason := savings(c, requested, usedName, streamRes, cost, now, swapClass)
		if cacheSaved > 0 {
			// Provider cache savings add to the routing savings, but the reason stays as
			// cache when there was no model swap.
			saved += cacheSaved
			if reason == "" {
				reason = "provider_prompt_cache"
			}
		}
		addSpend(ctx, c.budgetScope, cost)
		addRateTokens(ctx, c.limitsScope, c.Limits, tin+tout)
		addCreditSpend(ctx, usedProvider, c, dec, cost)
		if cacheStore.Enabled() && !noStore {
			full := map[string]interface{}{"id": id, "object": "chat.completion", "model": usedName,
				"choices": []map[string]interface{}{{"index": 0, "message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop"}},
				"usage":   map[string]int{"prompt_tokens": tin, "completion_tokens": tout, "total_tokens": tin + tout}}
			jb, _ := json.Marshal(full)
			storeTTL := effectiveCacheTTL(c)
			cacheStore.Put(ctx, ck, usedProvider, string(jb), cost, storeTTL)
			// Index the question's vector (reused from the read) for the semantic cache.
			if c.SemanticCache && len(semQueryVec) > 0 {
				indexSemantic(ctx, ck, semQueryVec, semQueryCtx, semQueryNum, storeTTL)
			}
		}
		rec := map[string]interface{}{"request_id": req.RequestID, "cache_key": ck, "team": team, "app_tag": app, "feature": feature, "provider": usedProvider, "upstream": usedUpstream, "model": usedName, "tokens_in": tin, "tokens_out": tout, "estimated_cost_usd": cost, "saved_usd": saved, "savings_reason": reason, "latency_ms": lat, "cache_hit": false, "status": "success", "category": "ok", "sli_eligible": true, "ts": time.Now().UTC().Format(time.RFC3339)}
		routing.DecorateUsage(rec, dec, requested, pricingStatus, cost, streamRes.cacheRead, streamRes.cacheWrite, streamRes.cacheConv)
		decorateSwap(rec, swapClass, servedModelID)
		decorateCanary(rec, canaryRoute)
		rec["price_source"] = priceSourceOf(c, usedName, now)
		routing.DecorateSavings(rec, saved, verifiedPortion(saved, cacheSaved, reason), reason)
		emitUsageFn(ctx, rec)
		return sresp(200, sseHeaders(reqOrigin), sb.String())
	}

	// --- Non-streaming (JSON) ---
	// Escalation: opt-in per feature, on by default when economy mode is active. One
	// retry at most, and the cost of both attempts is deducted from the savings in the
	// ledger — otherwise a feature that fails 30% of the time "saves" on paper while
	// spending more.
	escalateOn := escalationEnabled(c, feature)
	var lastErr error
	for _, s := range chain {
		res, err := callProviderFn(ctx, s.r, body.Messages, body.Tools)
		if err != nil {
			lastErr = err
			continue
		}
		attemptCost := 0.0
		escalated, escReason, escOutcome, attempts := false, "", "", 1
		usedName := s.name
		if escalateOn {
			e := maybeEscalate(ctx, c, s, body.Messages, body.Tools, maxTok, res, shape, now)
			res, usedName = e.res, e.model
			escalated, escReason, escOutcome, attempts, attemptCost =
				e.escalated, e.reason, e.outcome, e.attempts, e.totalCost
		}
		lat := int(time.Since(start).Milliseconds())
		caps := c.Routing[usedName].Capabilities
		cost, cacheSaved, pricingStatus := realizedCost(c, usedName, caps, res, now)
		if attemptCost > 0 {
			// Total cost = every attempt, not just the one that answered.
			cost = attemptCost
		}
		swapClass, servedModelID := servedSwap(c, requested, usedName)
		saved, reason := savings(c, requested, usedName, res, cost, now, swapClass)
		if cacheSaved > 0 {
			saved += cacheSaved
			if reason == "" {
				reason = "provider_prompt_cache"
			}
		}
		// The ledger's savings come ONLY from savings() (ex-post): it compares the cost
		// of the REQUESTED model over the RESPONSE's REAL TOKENS against the served
		// cost, and returns zero when there was no swap (used == requested). It is the
		// only honest comparison and it covers escalation, because `cost` here is
		// already the sum of the attempts — so a negative saving also lands at zero.
		//
		// Do NOT reintroduce here a credit based on dec.RequestedCostUSD (the EX-ANTE
		// estimate, with a HEURISTIC output): when the response comes out shorter than
		// the guess, the estimate-vs-real difference turned into PHANTOM
		// "auto_cheapest savings" even with no swap and with auto_cheapest off —
		// inflating the counterfactual ledger with token estimation error.
		usedRoute := c.Routing[usedName]

		// Assemble the response with tool_calls support
		finishReason := "stop"
		if res.stopReason != "" {
			// Normalize the stop reason to the OpenAI dialect
			switch res.stopReason {
			case "tool_use":
				finishReason = "tool_calls"
			case "end_turn":
				finishReason = "stop"
			default:
				finishReason = res.stopReason
			}
		}

		msgContent := map[string]interface{}{"role": "assistant", "content": res.text}
		if len(res.toolCalls) > 0 {
			msgContent["tool_calls"] = res.toolCalls
			if res.text == "" {
				msgContent["content"] = nil // OpenAI returns null when there are only tool_calls
			}
		}

		meta := map[string]interface{}{"team": team, "app_tag": app, "feature": feature, "provider": usedRoute.Provider, "model": usedName, "estimated_cost_usd": cost, "saved_usd": saved, "savings_reason": reason, "savings_class": routing.ClassOf(reason), "cache_hit": false, "latency_ms": lat, "auto_cheapest": c.AutoCheapest, "requested_model": requested, "budget_state": budgetState, "escalated": escalated}
		// The customer's own application is the first place that must be able to see
		// a substitution happened and how far it went — a client tuned for one model
		// may want to react (retry, flag the output, log it) instead of discovering
		// the swap later in a dashboard.
		decorateSwap(meta, swapClass, servedModelID)
		decorateCanary(meta, canaryRoute)
		out := map[string]interface{}{
			"id": "chatcmpl-" + ck[:12], "object": "chat.completion", "model": usedName,
			"choices": []map[string]interface{}{{"index": 0, "message": msgContent, "finish_reason": finishReason}},
			"usage":   map[string]int{"prompt_tokens": res.tin, "completion_tokens": res.tout, "total_tokens": res.tin + res.tout},
			"aiplat":  meta,
		}
		// Enforcement counters (month spend and tokens in the window).
		addSpend(ctx, c.budgetScope, cost)
		addRateTokens(ctx, c.limitsScope, c.Limits, res.tin+res.tout)
		addCreditSpend(ctx, usedRoute.Provider, c, dec, cost)
		// Do not cache a response with tool_calls (unique per context) nor a response
		// that failed validation — caching a broken response would serve it again.
		if cacheStore.Enabled() && !noStore && len(res.toolCalls) == 0 && escOutcome == "" {
			jb, _ := json.Marshal(out)
			storeTTL := effectiveCacheTTL(c)
			cacheStore.Put(ctx, ck, usedRoute.Provider, string(jb), cost, storeTTL)
			// Index the question's vector (reused from the read) for the semantic cache.
			if c.SemanticCache && len(semQueryVec) > 0 {
				indexSemantic(ctx, ck, semQueryVec, semQueryCtx, semQueryNum, storeTTL)
			}
		}
		rec := map[string]interface{}{"request_id": req.RequestID, "cache_key": ck, "team": team, "app_tag": app, "feature": feature, "provider": usedRoute.Provider, "upstream": upstreamOf(usedRoute), "model": usedName, "tokens_in": res.tin, "tokens_out": res.tout, "estimated_cost_usd": cost, "saved_usd": saved, "savings_reason": reason, "latency_ms": lat, "cache_hit": false, "status": "success", "category": "ok", "sli_eligible": true, "ts": time.Now().UTC().Format(time.RFC3339)}
		routing.DecorateUsage(rec, dec, requested, pricingStatus, cost, res.cacheRead, res.cacheWrite, res.cacheConv)
		decorateSwap(rec, swapClass, servedModelID)
		decorateCanary(rec, canaryRoute)
		rec["price_source"] = priceSourceOf(c, usedName, now)
		routing.DecorateSavings(rec, saved, verifiedPortion(saved, cacheSaved, reason), reason)
		routing.DecorateEscalation(rec, escalated, escReason, escOutcome, attempts, dec.RequestedCostUSD, cost)
		emitUsageFn(ctx, rec)
		return jbody(reqOrigin, 200, out)
	}
	emitFailure(ctx, req.RequestID, ident, feature, "", chosen, "error", classifyProviderErr(lastErr), fmt.Sprint(lastErr), int(time.Since(start).Milliseconds()))
	return jerr(reqOrigin, 502, "all providers failed: "+fmt.Sprint(lastErr))
}

// Deps are the production adapters the shell needs, constructed by cmd/router/main.go
// and injected via Wire. State ports are typed as PORTS (not concrete adapters):
// that is what allows an in-memory double in the orchestration tests (R3.2) and
// keeps this shell depending only on the boundary.
//
// Two fields are concrete on purpose: BedrockPool (client pooling for the
// cross-account AssumeRole is Bedrock-specific plumbing, not a boundary) and Sem
// (the semantic index shares the cache table and its entry format — a second port
// over the same adapter would be indirection with no substitution to buy).
type Deps struct {
	BedrockPool *bedrock.Pool
	Config      ports.ConfigStore
	Cache       ports.Cache
	Sem         *ddbcache.Store
	Embedder    ports.Embedder
	Limits      ports.LimitsStore
	Usage       ports.UsageSink
	Secrets     ports.SecretStore
	Keys        ports.KeyStore
	Hints       *ddbhints.Reader // nil = hints unavailable; the decision falls back to the heuristic

	// Org is the deployment's single org (DEPLOYMENT_ORG / Contract of Environment).
	// It is NOT used for config scoping here (ConfigStore already carries it) — the
	// one use is deriving the default BYO Bedrock ExternalID (see callProvider),
	// so a route left without an explicit external_id still gets a value scoped to
	// this deployment instead of a shared literal every customer would carry.
	Org string
}

// Wire installs the production dependencies. Called once, from main(), before any
// request is served. The package-level variables (rather than a struct threaded
// through every call) preserve the pre-move shape of the code byte for byte — the
// characterization tests swap individual seams and this keeps their surface intact.
func Wire(d Deps) {
	bedrockPool = d.BedrockPool
	configStore = d.Config
	cacheStore = d.Cache
	semStore = d.Sem
	embedder = d.Embedder
	limitsStore = d.Limits
	usageSink = d.Usage
	secretStore = d.Secrets
	keyStore = d.Keys
	hintsReader = d.Hints
	deploymentOrg = d.Org
}

// Handle is the exported entry point on the neutral boundary — what both wrappers
// (the Lambda adapter and the local HTTP server) converge on calling.
func Handle(ctx context.Context, req httpapi.Request) (httpapi.Response, error) {
	return handle(ctx, req)
}
