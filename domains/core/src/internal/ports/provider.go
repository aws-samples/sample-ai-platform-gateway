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
// hexagonal-refactor is meant to prevent.
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
	Model           string
	Messages        []Message
	Tools           []ToolDef
	MaxOutputTokens int
}

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
}

// Provider is the outbound port for an inference provider.
type Provider interface {
	Invoke(ctx context.Context, in InvokeInput) (Result, error)
}
