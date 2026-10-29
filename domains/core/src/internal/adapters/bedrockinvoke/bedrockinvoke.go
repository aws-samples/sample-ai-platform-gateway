// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package bedrockinvoke is the outbound adapter for Amazon Bedrock AgentCore Gateway's
// NATIVE InvokeModel inference path (/inference/model/{model}/invoke). It implements
// ports.Provider.
//
// # Why a sibling of bedrockgateway, not an extension of it
//
// The bedrockgateway adapter speaks the Anthropic Messages dialect on /inference/v1/messages
// and reaches only the Claude family, because that path rejects a model id containing ':'.
// Amazon Nova and the versioned Bedrock ids (ending "-v1:0") are unreachable there.
//
// Measured on the live gateway on 2026-10-26 (docs/agentcore-gateway-capabilities.md, B1/B2):
//
//   - The InvokeModel path carries the model id in the URL, so a ':' is a path segment, not a
//     rejected body field. us.amazon.nova-micro-v1:0 returns HTTP 200.
//   - The gateway's OpenAI-dialect path (/inference/v1/chat/completions) is NOT a clean reuse
//     for Nova (HTTP 400 "extraneous key [max_tokens]"), so openaicompat cannot be reused.
//   - The native path returns the family's native schema, which for Amazon Nova is the same
//     messages/inferenceConfig request and output.message/usage response that Bedrock's
//     Converse API uses, expressed as plain JSON. So this adapter maps the boundary types to
//     that JSON directly, with no aws-sdk bedrockruntime types, which also keeps it on the
//     right side of the ports/adapters boundary.
//
// # Scope
//
// This adapter handles the Amazon Nova family (text, system prompt, sampling parameters,
// token and prompt-cache usage counters). Meta Llama's native InvokeModel schema is
// different (prompt/generation) and is added only when a route needs it. Tool use, images
// and extended thinking on this path are a later increment, so Nova routes that use this
// provider are declared without those capabilities and the routing eligibility filter never
// sends such a request here.
//
// # Signing
//
// Both "bedrock-agentcore" and "bedrock" SigV4 signing names were accepted by the gateway
// (measured 2026-10-26). This adapter uses "bedrock-agentcore" so there is one signing name
// across every gateway transport.
package bedrockinvoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/aiplat/core/internal/ports"
)

// SigningService is the SigV4 service name for the gateway data plane. It is NOT "bedrock":
// the gateway is its own service. (The gateway also accepts "bedrock" on the InvokeModel
// path, but one signing name across transports is simpler to reason about.)
const SigningService = "bedrock-agentcore"

// invokePath builds the gateway's native InvokeModel path for a model id. The "/inference"
// prefix is what selects inference targets, and the id travels in the path, which is what
// lets a ':' through (see the package doc).
func invokePath(modelID string) string {
	return "/inference/model/" + modelID + "/invoke"
}

// Signer signs an outbound request with SigV4. An interface rather than the SDK signer
// directly, so a test can exercise the adapter without AWS credentials. Structurally
// identical to bedrockgateway.Signer, so the router's single gateway signer satisfies both.
type Signer interface {
	SignRequest(ctx context.Context, req *http.Request, payload []byte, service, region string) error
}

// Route is the per-route configuration this adapter needs.
type Route struct {
	// BaseURL is the gateway endpoint, e.g.
	// https://<gateway-id>.gateway.bedrock-agentcore.<region>.amazonaws.com
	BaseURL string
	// Region is the signing region. Empty falls back to the region in BaseURL's host.
	Region string
	// ModelID is the provider model id as it appears in the URL path, e.g.
	// "us.amazon.nova-micro-v1:0". It MAY contain ':'.
	ModelID string
	// PromptCache opts this route into the provider's prompt caching. For Amazon Nova the
	// native InvokeModel body carries a cachePoint on the system block, exactly as the
	// Converse body does.
	PromptCache bool
}

// Adapter implements ports.Provider over the native InvokeModel path.
type Adapter struct {
	HTTP    *http.Client
	Signer  Signer
	BaseURL string
	Region  string
	ModelID string
	// CachePrefix marks the end of the system block with a cache point when true.
	CachePrefix bool
}

var _ ports.Provider = (*Adapter)(nil) // compile-time assertion

// New builds the provider for a gateway InvokeModel route.
//
// It returns an error rather than a provider that skips signing, for the same reason as the
// bedrockgateway adapter: an unsigned request is a 403 that reads as a misconfigured
// service, and naming the real cause (nothing wired the signer) here is clearer.
func New(httpc *http.Client, signer Signer, r Route) (ports.Provider, error) {
	if r.BaseURL == "" {
		return nil, fmt.Errorf("bedrock_invoke: route has no base_url (gateway endpoint)")
	}
	if signer == nil {
		return nil, fmt.Errorf("bedrock_invoke: no SigV4 signer configured")
	}
	if r.ModelID == "" {
		return nil, fmt.Errorf("bedrock_invoke: route has no provider_model_id")
	}
	region := r.Region
	if region == "" {
		region = regionFromHost(r.BaseURL)
	}
	if region == "" {
		return nil, fmt.Errorf("bedrock_invoke: cannot determine signing region from base_url %q; set region on the route", r.BaseURL)
	}
	if httpc == nil {
		httpc = http.DefaultClient
	}
	return &Adapter{
		HTTP:        httpc,
		Signer:      signer,
		BaseURL:     strings.TrimSuffix(r.BaseURL, "/"),
		Region:      region,
		ModelID:     r.ModelID,
		CachePrefix: r.PromptCache,
	}, nil
}

func (a *Adapter) Invoke(ctx context.Context, in ports.InvokeInput) (ports.Result, error) {
	payload, err := json.Marshal(buildBody(in, a.CachePrefix))
	if err != nil {
		return ports.Result{}, fmt.Errorf("bedrock_invoke: marshal request: %w", err)
	}
	endpoint := a.BaseURL + invokePath(a.ModelID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return ports.Result{}, fmt.Errorf("bedrock_invoke: build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	if err := a.Signer.SignRequest(ctx, req, payload, SigningService, a.Region); err != nil {
		return ports.Result{}, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return ports.Result{}, fmt.Errorf("bedrock_invoke: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Surface the status and a short, content-free slice of the body so a provider
		// failure is classified correctly upstream (classifyProviderErr reads the status).
		return ports.Result{}, fmt.Errorf("bedrock_invoke: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	return parseBody(body)
}

// --- request mapping (pure) --------------------------------------------------

// The native Amazon Nova InvokeModel body is the Converse body in JSON form: a top-level
// system array, a messages array of {role, content:[{text}]}, and an inferenceConfig. These
// structs produce exactly that shape. They are deliberately minimal: fields this adapter
// does not yet support (tools, images) are not emitted, because a Nova route is declared
// without those capabilities so the request never carries them.

type novaBody struct {
	System          []novaText     `json:"system,omitempty"`
	Messages        []novaMessage  `json:"messages"`
	InferenceConfig *novaInference `json:"inferenceConfig,omitempty"`
}

type novaMessage struct {
	Role    string      `json:"role"`
	Content []novaBlock `json:"content"`
}

type novaBlock struct {
	Text       string          `json:"text,omitempty"`
	CachePoint *novaCachePoint `json:"cachePoint,omitempty"`
}

type novaText struct {
	Text       string          `json:"text,omitempty"`
	CachePoint *novaCachePoint `json:"cachePoint,omitempty"`
}

type novaCachePoint struct {
	Type string `json:"type"`
}

type novaInference struct {
	MaxTokens     *int     `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

// buildBody maps the boundary request into the Nova native InvokeModel JSON.
//
// System messages are collected into the top-level system array (Nova keeps system
// separate from the conversation, like Converse). Every other message becomes a
// conversation turn; its role is normalized to "user" or "assistant" because Nova accepts
// only those two in the messages array. Empty-text messages are skipped so an assistant
// turn that only carried tool calls (not supported here) does not produce an empty block.
func buildBody(in ports.InvokeInput, cachePrefix bool) novaBody {
	b := novaBody{}
	for _, m := range in.Messages {
		if m.Role == "system" {
			if m.Text != "" {
				b.System = append(b.System, novaText{Text: m.Text})
			}
			continue
		}
		if m.Text == "" {
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		b.Messages = append(b.Messages, novaMessage{
			Role:    role,
			Content: []novaBlock{{Text: m.Text}},
		})
	}
	// Prompt cache: mark the end of the system block with a cache point, exactly as the
	// Converse adapter does. Only meaningful when there is a system prefix to cache.
	if cachePrefix && len(b.System) > 0 {
		b.System = append(b.System, novaText{CachePoint: &novaCachePoint{Type: "default"}})
	}
	if cfg := buildInference(in); cfg != nil {
		b.InferenceConfig = cfg
	}
	return b
}

// buildInference fills inferenceConfig, every field conditional. A zero we invented is not
// neutral: temperature 0 makes the model deterministic and maxTokens 0 is rejected, so
// "the client said nothing" must reach the provider as an absent field. Same rule the
// Converse adapter's applyInference follows.
func buildInference(in ports.InvokeInput) *novaInference {
	var cfg *novaInference
	ensure := func() *novaInference {
		if cfg == nil {
			cfg = &novaInference{}
		}
		return cfg
	}
	if in.MaxOutputTokens > 0 {
		mt := in.MaxOutputTokens
		ensure().MaxTokens = &mt
	}
	if t := in.Inference.Temperature; t != nil {
		v := *t
		ensure().Temperature = &v
	}
	if p := in.Inference.TopP; p != nil {
		v := *p
		ensure().TopP = &v
	}
	if len(in.Inference.Stop) > 0 {
		ensure().StopSequences = in.Inference.Stop
	}
	return cfg
}

// --- response mapping (pure) -------------------------------------------------

type novaResponse struct {
	Output struct {
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string `json:"stopReason"`
	Usage      *struct {
		InputTokens           *int `json:"inputTokens"`
		OutputTokens          *int `json:"outputTokens"`
		TotalTokens           *int `json:"totalTokens"`
		CacheReadInputTokens  *int `json:"cacheReadInputTokenCount"`
		CacheWriteInputTokens *int `json:"cacheWriteInputTokenCount"`
	} `json:"usage"`
}

// parseBody turns the native Nova InvokeModel response into a ports.Result.
//
// The cache counters use the native field names (cacheReadInputTokenCount, not the Messages
// path's cache_read_input_tokens). Nova always reports them, so their presence is
// CacheCountersReported, and a missing usage block is Absent, preserving the
// reported-vs-absent distinction the cost model relies on.
func parseBody(body []byte) (ports.Result, error) {
	var r novaResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return ports.Result{}, fmt.Errorf("bedrock_invoke: decode response: %w", err)
	}
	var sb strings.Builder
	for _, c := range r.Output.Message.Content {
		sb.WriteString(c.Text)
	}
	res := ports.Result{
		Text:          sb.String(),
		StopReason:    r.StopReason,
		CacheCounters: ports.CacheCountersAbsent,
	}
	if r.Usage != nil {
		if r.Usage.InputTokens != nil {
			res.InputTokens = *r.Usage.InputTokens
		}
		if r.Usage.OutputTokens != nil {
			res.OutputTokens = *r.Usage.OutputTokens
		}
		if r.Usage.CacheReadInputTokens != nil || r.Usage.CacheWriteInputTokens != nil {
			res.CacheReadInputTokens = deref(r.Usage.CacheReadInputTokens)
			res.CacheWriteInputTokens = deref(r.Usage.CacheWriteInputTokens)
			res.CacheCounters = ports.CacheCountersReported
		}
	}
	return res, nil
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// regionFromHost extracts the region from a gateway endpoint host.
// "<gateway-id>.gateway.bedrock-agentcore.<region>.amazonaws.com".
func regionFromHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	parts := strings.Split(u.Hostname(), ".")
	for i, p := range parts {
		if p == "bedrock-agentcore" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}
