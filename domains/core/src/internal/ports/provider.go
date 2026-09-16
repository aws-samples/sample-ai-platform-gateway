// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ports declares the outbound boundaries of the Core domain.
//
// Rule: this package does NOT import SDKs, network or environment. It is imported
// by the pure domain (internal/routing), so any infrastructure dependency here
// would leak past the boundary verified by internal/routing/boundary_test.go.
package ports

import "context"

// ToolDef is a tool in the OpenAI dialect (what the client sends).
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]interface{} // JSON Schema
}

// ToolCall is a tool invocation requested by the model.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // stringified JSON, as in the OpenAI dialect
}

// ImagePart is a decoded image extracted from a multimodal message's `content`
// array (the OpenAI dialect's `{"type":"image_url","image_url":{"url":"data:..."}}`
// part). Format is the bare image format ("png","jpeg","gif","webp" — no "image/"
// prefix, no ";base64"), Bytes is the already base64-DECODED image data.
//
// This exists as its own boundary type (rather than leaving adapters to re-parse
// Raw) because every adapter that wants to send the image needs the same decode —
// duplicating a base64/data-URL parser per adapter is exactly the kind of drift
// the hexagonal split is meant to prevent.
type ImagePart struct {
	Format string
	Bytes  []byte
}

// Message is the BOUNDARY type for a conversation message.
//
// Raw keeps the original `content` because the OpenAI spec accepts a string, null
// (an assistant that only returns tool_calls) and an array of multimodal parts.
// Text is the textual projection, which is what the domain uses to count tokens
// and apply guardrails — the domain never needs the structured content.
// Images carries any image parts found in that same array, already decoded —
// see hasImage/extractImages in the gateway shell for how they are found.
type Message struct {
	Role   string
	Text   string
	Raw    []byte
	Images []ImagePart
	// Name is the OpenAI dialect `name` (the function name on a message with role
	// tool). Preserved at the boundary because the adapter rebuilds the wire
	// format from here and omitting it would change the body sent to the provider.
	Name       string
	ToolCalls  []ToolCall
	ToolCallID string
}

// InvokeInput is the request to a provider. Model is the LOGICAL name (the model
// catalog key), not the provider id — the translation belongs to the adapter.
type InvokeInput struct {
	Model    string
	Messages []Message
	Tools    []ToolDef
	// MaxOutputTokens is the client's output ceiling (0 = not informed).
	//
	// It lived here UNREAD by every adapter: the field existed, the routing layer used it
	// for the context-window check and the cost estimate, and then nobody sent it to the
	// provider. A client asking for max_tokens: 100 to cap its spend was billed for
	// whatever the model felt like producing — measured on a live call, 400 requested came
	// back as 560 completion tokens. Adapters MUST forward it now.
	MaxOutputTokens int
	// Inference carries the rest of the sampling parameters and the reasoning opt-in.
	// Grouped in one struct rather than added as four more fields so that adding the next
	// parameter does not touch five call sites again.
	Inference InferenceParams
}

// InferenceParams are the per-request sampling controls, in provider-neutral form.
//
// POINTERS for the scalars, matching the convention the HTTP boundary and the cache key
// already use: temperature 0 is a meaningful, deterministic request and must not be
// indistinguishable from "the client said nothing". Sending a zero we invented would
// silently change a customer's output.
type InferenceParams struct {
	Temperature *float64
	TopP        *float64
	// Stop are the stop sequences. Empty means the client sent none — never send an empty
	// array to a provider, since some read that as "stop immediately".
	Stop []string
	// Reasoning is the extended-thinking request, already RESOLVED and CLAMPED by the
	// gateway (see gateway.resolveReasoning). nil means the client did not ask to think,
	// which is not the same as asking not to: see ReasoningRequest.Disabled.
	Reasoning *ReasoningRequest
}

// ReasoningRequest is a resolved extended-thinking request.
//
// The client speaks in EFFORT (`reasoning_effort: low|medium|high|none`, the OpenAI
// dialect) and adapters need a NUMBER, because Bedrock, Anthropic and Google all take a
// token budget. Resolving effort → budget once in the gateway rather than in each adapter
// is what keeps four adapters from inventing four different meanings of "high", and it is
// also the only place that can apply the operator's ceiling.
//
// Effort is carried alongside the budget because one dialect wants the word back:
// OpenAI-compatible providers take `reasoning_effort` directly and would lose information
// if we handed them a token count they never asked for.
type ReasoningRequest struct {
	// Effort is the client's word, normalized. "" when the client gave only a budget.
	Effort string
	// BudgetTokens is the thinking allowance, already clamped to the operator ceiling.
	// Zero with Disabled false means "the provider decides".
	BudgetTokens int
	// Disabled is an EXPLICIT request not to think, which some providers honour (a zero
	// thinking budget on Gemini, minimal effort on OpenAI). It is a separate field because
	// BudgetTokens == 0 already means "unspecified", and conflating the two would turn
	// "you choose" into "never think" for every request that omitted a number.
	Disabled bool
}

// Effort levels of the OpenAI dialect, normalized.
const (
	ReasoningEffortNone   = "none"
	ReasoningEffortLow    = "low"
	ReasoningEffortMedium = "medium"
	ReasoningEffortHigh   = "high"
)

// Cache token counters reported (or not) by the provider.
const (
	CacheCountersReported = "reported"
	CacheCountersAbsent   = "absent"
)

// Result is the normalized response from a provider.
//
// CacheCounters distinguishes "the provider reported zero" from "the provider
// does not report this data". The difference matters: in the first case the cache
// was not used, in the second we do not know, and treating both as zero would
// hide real savings.
type Result struct {
	Text                  string
	InputTokens           int
	OutputTokens          int
	CacheReadInputTokens  int
	CacheWriteInputTokens int
	CacheCounters         string
	ToolCalls             []ToolCall
	StopReason            string

	// Reasoning is the model's chain of thought, when the model emits one and the
	// provider exposes it. It is kept SEPARATE from Text on purpose: concatenating the
	// two would put thinking inside the answer, and no caller asked for that.
	//
	// Reasoning tokens are NOT reported separately by Bedrock — TokenUsage has only
	// input/output/total — so they are already inside OutputTokens and therefore already
	// billed and already in the ledger. What this field adds is visibility into how much
	// of the output was thinking, which is why ReasoningChars exists next to it rather
	// than a token count we would have to invent.
	Reasoning string
	// ReasoningChars counts the reasoning characters observed on the wire. Deliberately
	// characters and not tokens: the provider does not give a reasoning token count, and
	// deriving one would be a guess presented as a measurement.
	ReasoningChars int
	// ReasoningSignature is the provider's cryptographic signature over the thinking
	// block. It is what allows a reasoning chain to CONTINUE across turns, so dropping it
	// silently would make any future conversation store useless for reasoning models —
	// the text would be there and the chain still unusable.
	ReasoningSignature string
	// ReasoningRedacted is true when the provider returned encrypted/redacted thinking
	// instead of readable text. Recorded rather than ignored so "the model did not think"
	// is distinguishable from "we were not allowed to see it".
	ReasoningRedacted bool
}

// Chunk is one increment of a provider stream: answer text, reasoning text, or the
// metadata that closes a reasoning block. Exactly one of the text fields is normally set.
type Chunk struct {
	// Text is answer content, destined for delta.content.
	Text string
	// Reasoning is chain-of-thought content, destined for delta.reasoning_content. It
	// must never be merged into Text — see Result.Reasoning.
	Reasoning string
	// Signature closes a reasoning block. Carried through so multi-turn reasoning
	// continuity stays possible; it is not forwarded to the client as content.
	Signature string
	// Redacted marks encrypted thinking: there is content, we just cannot read it.
	Redacted bool
}

// Provider is the outbound port for an inference provider.
type Provider interface {
	Invoke(ctx context.Context, in InvokeInput) (Result, error)
}

// ProviderStream is an OPEN provider stream, drained one text delta at a time.
//
// It is split from opening on purpose (see StreamProvider): the handler commits the
// client's HTTP status before the first payload byte, so everything that could still
// change that status has to happen while nothing has been written.
//
// Recv returns the next text delta. io.EOF means the model finished normally; any other
// error means the stream broke mid-answer, and the caller keeps what it already received
// rather than discarding it — the tokens were served and have to be billed.
//
// Result is only complete after Recv has returned an error. Providers report usage in a
// trailing event, so asking earlier gives partial counts.
type ProviderStream interface {
	Recv() (string, error)
	Result() Result
	Close() error
}

// ReasoningStream is the OPTIONAL chain-of-thought half of ProviderStream.
//
// Optional for the same reason StreamProvider is: an adapter implements it only where the
// provider actually exposes reasoning, and the pump type-asserts for it. A stream without
// it behaves exactly as before.
//
// Why a second method instead of changing Recv's signature: Recv returns a plain string and
// is implemented by every adapter, including the shared SSE helper used by providers this
// deployment cannot exercise. Widening the shared signature would force edits into paths
// that have no live coverage here, to add a field they do not produce. The type assertion
// keeps the blast radius on the one adapter that has the data.
//
// RecvChunk has the same contract as Recv: io.EOF means a normal end, any other error means
// the stream broke mid-answer and the caller keeps what it already received.
type ReasoningStream interface {
	RecvChunk() (Chunk, error)
}

// StreamProvider is the OPTIONAL native-streaming half of Provider.
//
// Optional on purpose: adapters implement it only where the provider has a real streaming
// API, and the handler type-asserts for it. A provider without it still streams to the
// client — the handler fetches the complete answer and slices it into frames — it just
// cannot lower time-to-first-token.
//
// OpenStream must not return a stream and an error together, and must validate whatever
// the provider validates up front (auth, model id, request shape) so a failure can still
// fall back to another route.
type StreamProvider interface {
	OpenStream(ctx context.Context, in InvokeInput) (ProviderStream, error)
}
