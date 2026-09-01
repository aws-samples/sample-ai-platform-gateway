// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Cache key — PURE DOMAIN (semantic-cache-agent, Phase 0 and 1).
//
// Two things live here, both deterministic and property-testable:
//
//  1. KEY CORRECTNESS (Phase 0, R2): the key now includes EVERYTHING that changes
//     the response — tools, temperature and max_tokens — besides org, model and
//     messages. Before, the key was sha256(org|model|messages); a request WITH
//     tools could receive the text response of one WITHOUT tools, and varying the
//     creativity (temperature) served the deterministic response that was stored.
//     temperature/max_tokens are POINTERS: "not informed" ≠ "zero".
//
//  2. CANONICAL KEY (Phase 1, R1): in `canonical` mode, the message TEXT is
//     normalized (lowercase, no accents, collapsed whitespace, no trailing
//     punctuation) BEFORE hashing, so that "Férias?" and "ferias" collide. This
//     affects ONLY the key — the body sent to the provider is always the original (P2).
//
// The org goes at the start of the hashed material: the cache NEVER crosses orgs (P3).
//
// Boundary: crypto/sha256 and encoding/hex are PURE computation (no IO, clock or
// randomness) and are on the boundary_test allowlist for that reason.
package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/aiplat/core/internal/ports"
)

// KeyMode selects the key strategy.
type KeyMode string

const (
	// KeyExact: byte match (after the tools/temperature/max_tokens correction).
	KeyExact KeyMode = "exact"
	// KeyCanonical: match on the normalized form of the text (case/accent/space/punctuation).
	KeyCanonical KeyMode = "canonical"
)

// NormalizeKeyMode returns a valid KeyMode; any unknown or empty value falls back to
// KeyExact (the safe default — it collapses nothing without opt-in).
func NormalizeKeyMode(s string) KeyMode {
	if KeyMode(s) == KeyCanonical {
		return KeyCanonical
	}
	return KeyExact
}

// KeyInput is everything that CAN change the response. If a field changes the
// response and is not here, the cache swaps semantics — which was exactly the
// defect with tools.
type KeyInput struct {
	Org         string
	Model       string
	Messages    []ports.Message
	Tools       []ports.ToolDef
	Temperature *float64 // pointer: "not informed" ≠ "0" (deterministic)
	MaxTokens   *int
}

// keyMsg is the projection of the message that enters the hash material. Content is
// the ORIGINAL content (RawMessage: string, null or multimodal array) in exact mode;
// in canonical mode it is the normalized form of the text.
type keyMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	CanonText  string          `json:"canon,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []keyToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type keyToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

type keyTool struct {
	Name   string          `json:"name"`
	Desc   string          `json:"desc,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// keyMaterial is the deterministic object that goes into sha256. The field order is
// fixed (a struct), and json.Marshal sorts map keys — so the hash is stable.
type keyMaterial struct {
	Mode     string    `json:"mode"`
	Org      string    `json:"org"`
	Model    string    `json:"model"`
	Messages []keyMsg  `json:"messages"`
	Tools    []keyTool `json:"tools,omitempty"`
	Temp     *float64  `json:"temperature,omitempty"`
	MaxTok   *int      `json:"max_tokens,omitempty"`
}

// CacheKey returns the cache key for the request, in the given mode.
func CacheKey(in KeyInput, mode KeyMode) string {
	canonical := mode == KeyCanonical
	m := keyMaterial{
		Mode: string(mode), Org: in.Org, Model: in.Model,
		Temp: in.Temperature, MaxTok: in.MaxTokens,
	}
	m.Messages = make([]keyMsg, len(in.Messages))
	for i, msg := range in.Messages {
		km := keyMsg{Role: msg.Role, Name: msg.Name, ToolCallID: msg.ToolCallID}
		if canonical {
			// Only the normalized textual projection enters the key. Structure
			// (multimodal/null) collapses into the text — in canonical mode what
			// matters is the textual meaning of the question.
			km.CanonText = Canonicalize(msg.Text)
		} else if len(msg.Raw) > 0 {
			km.Content = json.RawMessage(msg.Raw)
		} else if msg.Text != "" {
			b, _ := json.Marshal(msg.Text)
			km.Content = b
		}
		for _, tc := range msg.ToolCalls {
			km.ToolCalls = append(km.ToolCalls, keyToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Arguments})
		}
		m.Messages[i] = km
	}
	for _, t := range in.Tools {
		kt := keyTool{Name: t.Name, Desc: t.Description}
		if t.Parameters != nil {
			if b, err := json.Marshal(t.Parameters); err == nil {
				kt.Params = b
			}
		}
		m.Tools = append(m.Tools, kt)
	}
	b, _ := json.Marshal(m)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

var reWhitespace = regexp.MustCompile(`\s+`)

// Canonicalize normalizes text for the canonical key. Deterministic and idempotent.
// Steps: lowercase → fold Latin diacritics → collapse whitespace → strip trailing
// punctuation.
//
// DECLARED DEVIATION from the design (R1.2 asked for NFKC): Unicode NFKC
// normalization requires golang.org/x/text/unicode/norm — an external dependency the
// domain boundary does not allow and that this environment's network does not
// download reliably. The Latin fold below covers the real case for a PT/ES audience
// ("férias" ≡ "ferias", "ção" ≡ "cao"); full Unicode folding waits until x/text
// comes in.
func Canonicalize(s string) string {
	s = strings.ToLower(s)
	s = foldDiacritics(s)
	s = reWhitespace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ".?!,;: \t\n")
	return strings.TrimSpace(s)
}

// latinFold maps common accented Latin letters (PT/ES/base) to their unaccented
// form. Already lowercase (Canonicalize applies ToLower first).
var latinFold = map[rune]rune{
	'á': 'a', 'à': 'a', 'ã': 'a', 'â': 'a', 'ä': 'a', 'å': 'a',
	'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e',
	'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i',
	'ó': 'o', 'ò': 'o', 'õ': 'o', 'ô': 'o', 'ö': 'o',
	'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u',
	'ç': 'c', 'ñ': 'n', 'ý': 'y', 'ÿ': 'y',
}

func foldDiacritics(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if f, ok := latinFold[r]; ok {
			b.WriteRune(f)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
