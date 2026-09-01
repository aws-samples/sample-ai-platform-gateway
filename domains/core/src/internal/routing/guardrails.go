// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Content guardrails — PURE DOMAIN (hexagonal-refactor, task 5).
//
// PII masking and secret/injection detection are DETERMINISTIC rules. They used to live
// in the shell (cmd/router), with no property at all testing them — exactly the kind of
// rule where an uncovered case means leaking personal data to the provider. Moving them
// here makes them property-testable and keeps the clock out of the game (they are pure
// string functions).
//
// The patterns were NOT altered in the move (Req 12.1): they are the same regexes the
// shell applied. `regexp` is pure (no IO, no clock, no randomness), which is why it is
// on the boundary allowlist.
package routing

import (
	"encoding/json"
	"regexp"

	"github.com/aiplat/core/internal/ports"
)

var (
	reEmail = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	reCPF   = regexp.MustCompile(`\b\d{3}\.?\d{3}\.?\d{3}-?\d{2}\b`)
	reCard  = regexp.MustCompile(`\b(?:\d[ -]?){13,16}\b`)
	rePhone = regexp.MustCompile(`\b\(?\d{2}\)?[ -]?\d{4,5}-?\d{4}\b`)
	// US Social Security Number (###-##-####). Loose on the separator (dash or
	// space, matching the phone/CPF patterns' tolerance) but anchored to the
	// 3-2-4 digit grouping — that shape is specific enough to not collide with
	// CPF (3-3-3-2) or phone (2 + 4-5 + 4).
	reSSN = regexp.MustCompile(`\b\d{3}[ -]?\d{2}[ -]?\d{4}\b`)
	// Secrets: OpenAI/AWS keys, Bearer tokens, long hex.
	reSecret = regexp.MustCompile(`(?i)(sk-[a-z0-9]{16,}|AKIA[0-9A-Z]{16}|bearer\s+[a-z0-9._\-]{20,}|\b[0-9a-f]{40,}\b)`)
	// Prompt injection: classic patterns (heuristic, not a replacement for a model).
	reInjection = regexp.MustCompile(`(?i)(ignore (all |the )?(previous|above) instructions|disregard the (system|above)|reveal your (system )?prompt|you are now [a-z]|forget your (rules|instructions))`)
)

// MaskPII masks email/CPF/card/phone/SSN. Idempotent: the replacement markers
// ([email], [cpf], ...) do not match any of the patterns, so masking twice is the same
// as masking once.
//
// SSN runs LAST on purpose: its 3-2-4 digit grouping is loose enough (optional
// separators) to also match the tail of some phone-number inputs (e.g. an
// 11-digit "98765-4321" local number). Running phone/CPF/card first lets them
// claim their own longer digit runs before SSN's shorter, looser pattern gets
// a chance — preserving the pre-existing patterns' behavior byte for byte
// (Req 12.1) instead of the new pattern changing what a phone number masks as.
func MaskPII(s string) string {
	s = reEmail.ReplaceAllString(s, "[email]")
	s = reCPF.ReplaceAllString(s, "[cpf]")
	s = reCard.ReplaceAllString(s, "[cartao]")
	s = rePhone.ReplaceAllString(s, "[telefone]")
	s = reSSN.ReplaceAllString(s, "[ssn]")
	return s
}

// ContainsSecret tells whether the text contains a known secret/key.
func ContainsSecret(s string) bool { return reSecret.MatchString(s) }

// LooksLikeInjection applies the prompt injection heuristic (classic patterns).
func LooksLikeInjection(s string) bool { return reInjection.MatchString(s) }

// GuardrailPolicy is the slice of content policy the guardrail decision needs. NoStore
// does not affect the content (it is a cache decision), but it lives here so the
// effective scope is a single object.
type GuardrailPolicy struct {
	MaskPII        bool
	BlockSecrets   bool
	BlockInjection bool
	NoStore        bool
}

// ApplyGuardrails applies the policy to the set of messages. Blocking takes PRECEDENCE
// over masking (if there is a secret/injection, refuse before masking).
// Returns (possibly masked messages, block reason or "").
//
// The evaluated text is the textual projection (Message.Text); when masking, Text and Raw
// are rewritten with the masked string — the same semantics the shell applied (the
// content becomes the masked string, flattening multimodal, inherited behavior).
func ApplyGuardrails(msgs []ports.Message, p GuardrailPolicy) ([]ports.Message, string) {
	for _, m := range msgs {
		c := m.Text
		if p.BlockSecrets && ContainsSecret(c) {
			return msgs, "secret_detected"
		}
		if p.BlockInjection && LooksLikeInjection(c) {
			return msgs, "prompt_injection"
		}
	}
	if p.MaskPII {
		out := make([]ports.Message, len(msgs))
		for i, m := range msgs {
			masked := MaskPII(m.Text)
			m.Text = masked
			b, _ := json.Marshal(masked)
			m.Raw = b
			out[i] = m
		}
		return out, ""
	}
	return msgs, ""
}
