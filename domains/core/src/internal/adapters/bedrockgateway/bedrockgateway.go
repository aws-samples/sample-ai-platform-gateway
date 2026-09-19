// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package bedrockgateway is the outbound adapter for Amazon Bedrock AgentCore Gateway's
// inference targets. It implements ports.Provider through the anthropic package.
//
// # Why this is a transport and not a translation
//
// AgentCore Gateway exposes three inference paths — /v1/messages, /v1/chat/completions and
// /v1/responses — and routes each request to a target based on the `model` field. On
// /v1/messages it speaks the Anthropic Messages dialect verbatim, including the parts that
// are easy to get wrong: tool_result blocks grouped into one user turn, thinking blocks as
// their own content type, cache_control on the system block, and the two-event streaming
// usage accounting. So this package configures the anthropic adapter rather than
// reimplementing it; what it owns is the endpoint, SigV4 auth, and the model-id form.
//
// # Why /v1/messages and not /v1/chat/completions
//
// Measured against the live service: on the chat-completions path the usage block carries
// only {prompt_tokens, completion_tokens, total_tokens} — no cache fields at all — and
// Claude rejects that path outright ("does not support the '/v1/chat/completions' API").
// On /v1/messages the counters come back byte-identical to a direct Bedrock call
// (cache_creation_input_tokens then cache_read_input_tokens for the same prefix), which is
// what keeps the savings ledger honest for routes served this way. Choosing the other path
// would not fail — it would silently report every request as a cache miss.
//
// # Model ids
//
// The gateway's own validation REJECTS a model id containing ':' ("Model ID contains
// invalid characters"), so the versioned Bedrock ids that end in "-v1:0" cannot be used
// here. What works is the versionless family id, optionally qualified by target name:
// "us.anthropic.claude-opus-4-8", or "mytarget/anthropic.claude-haiku-4-5" to pin a
// specific target when several match. That is configuration (provider_model_id), not
// something this adapter can paper over — an id it rewrote would be an id the operator
// did not choose.
package bedrockgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/aiplat/core/internal/adapters/anthropic"
	"github.com/aiplat/core/internal/ports"
)

// SigningService is the SigV4 service name for the gateway's data plane. It is NOT
// "bedrock": the gateway is its own service and a signature scoped to bedrock is rejected.
const SigningService = "bedrock-agentcore"

// MessagesPath is the gateway's Anthropic-dialect inference path. The "/inference" prefix
// is what selects inference targets over MCP/HTTP ones; without it the gateway answers
// "No Target found for Target name: v1".
const MessagesPath = "/inference/v1/messages"

// BodyVersion is the API version the Bedrock family requires in the request body.
const BodyVersion = "bedrock-2023-05-31"

// Signer signs an outbound request with SigV4.
//
// An interface rather than the SDK signer directly so a test can exercise the adapter
// without AWS credentials, the same reason bedrock's converseAPI seam exists. It is NOT
// optional: see New.
type Signer interface {
	SignRequest(ctx context.Context, req *http.Request, payload []byte, service, region string) error
}

// SigV4 is the production Signer, over the deployment's own credentials.
//
// The gateway holds its OWN outbound execution role (one per gateway, shared by its
// targets), so there is no per-route AssumeRole here and no BYO-Bedrock equivalent: what
// this signs is the deployment's identity calling the gateway, and the gateway's role is
// what calls the model. That is a real difference from the bedrock adapter and it is the
// reason a customer's own Bedrock account cannot be reached through this provider.
type SigV4 struct {
	Creds aws.CredentialsProvider
	// Region is the fallback when a route names none.
	Region string

	signer *v4.Signer
}

// NewSigV4 builds a Signer from the process's AWS configuration.
func NewSigV4(cfg aws.Config) *SigV4 {
	return &SigV4{Creds: cfg.Credentials, Region: cfg.Region, signer: v4.NewSigner()}
}

// SignRequest implements Signer.
func (s *SigV4) SignRequest(ctx context.Context, req *http.Request, payload []byte, service, region string) error {
	if s.Creds == nil {
		return fmt.Errorf("bedrock_gateway: no credentials available for SigV4")
	}
	creds, err := s.Creds.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("bedrock_gateway: retrieving credentials: %w", err)
	}
	sum := sha256.Sum256(payload)
	sg := s.signer
	if sg == nil {
		sg = v4.NewSigner()
	}
	// The payload hash is passed explicitly rather than letting the signer read the body:
	// req.Body is a one-shot reader and consuming it here would send an empty request.
	return sg.SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), service, region, time.Now())
}

// Route is the per-route configuration this adapter needs, projected out of the gateway's
// Route so this package does not depend on the gateway package.
type Route struct {
	// BaseURL is the gateway endpoint, e.g.
	// https://<gateway-id>.gateway.bedrock-agentcore.<region>.amazonaws.com
	BaseURL string
	// Region is the signing region. Empty falls back to the region in BaseURL's host, then
	// to the signer's default.
	Region string
	// ModelID is the provider model id, target-qualified or not. See the package doc for
	// the ':' restriction.
	ModelID string
	// PromptCache opts this route into the provider's prompt caching, exactly as the
	// bedrock and anthropic routes do.
	PromptCache bool
	// ReasoningStyle declares which extended-thinking shape this MODEL accepts. It is a
	// per-model fact and the two shapes are mutually exclusive, so it cannot be defaulted
	// for a gateway that fronts several model families at once — see
	// anthropic.ReasoningStyleBudget / ReasoningStyleAdaptive.
	ReasoningStyle string
}

// New builds the provider for a gateway route.
//
// It returns an error rather than a provider that skips signing when no Signer is
// available. An unsigned request would be rejected by the gateway with a 403, so the
// failure is not silent either way — but reporting it here names the actual problem
// (nothing wired the signer) instead of surfacing as an auth error against a service the
// operator believes is misconfigured.
func New(httpc *http.Client, signer Signer, r Route) (ports.Provider, error) {
	if r.BaseURL == "" {
		return nil, fmt.Errorf("bedrock_gateway: route has no base_url (gateway endpoint)")
	}
	if signer == nil {
		return nil, fmt.Errorf("bedrock_gateway: no SigV4 signer configured")
	}
	region := r.Region
	if region == "" {
		region = regionFromHost(r.BaseURL)
	}
	if region == "" {
		return nil, fmt.Errorf("bedrock_gateway: cannot determine signing region from base_url %q; set region on the route", r.BaseURL)
	}
	return &anthropic.Adapter{
		HTTP:        httpc,
		BaseURL:     strings.TrimSuffix(r.BaseURL, "/"),
		ModelID:     r.ModelID,
		CachePrefix: r.PromptCache,
		Transport: anthropic.Transport{
			Path:           MessagesPath,
			BodyVersion:    BodyVersion,
			Label:          "bedrock_gateway",
			ReasoningStyle: r.ReasoningStyle,
			Sign: func(ctx context.Context, req *http.Request, payload []byte) error {
				return signer.SignRequest(ctx, req, payload, SigningService, region)
			},
		},
	}, nil
}

// regionFromHost extracts the region from a gateway endpoint host.
//
// The host is "<gateway-id>.gateway.bedrock-agentcore.<region>.amazonaws.com", so the
// region is the label after "bedrock-agentcore". Deriving it beats requiring the operator
// to state it twice and disagree with themselves — a region mismatch produces a signature
// the service rejects, which reads as a credentials problem.
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
