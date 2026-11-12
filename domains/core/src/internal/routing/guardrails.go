// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Content guardrails — PURE DOMAIN.
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
	// Secrets — named provider token shapes. sk- allows hyphens so sk-proj-… matches;
	// GitHub (ghp_/gho_/ghu_/ghs_/ghr_ + github_pat_), Slack (xox[baprs]-), Bearer tokens,
	// and JWTs (three base64url segments) are all common leak shapes the old rule missed.
	reSecret = regexp.MustCompile(`(?i)(` +
		`sk-[a-z0-9_\-]{16,}` + `|` +
		`AKIA[0-9A-Z]{16}` + `|` +
		`gh[opsur]_[a-z0-9]{20,}` + `|` +
		`github_pat_[a-z0-9_]{20,}` + `|` +
		`xox[baprs]-[a-z0-9\-]{10,}` + `|` +
		`bearer\s+[a-z0-9._\-]{20,}` + `|` +
		`eyJ[a-z0-9_\-]{8,}\.eyJ[a-z0-9_\-]{8,}\.[a-z0-9_\-]{5,}` +
		`)`)
	// PEM private-key headers (RSA/EC/OPENSSH/plain) — a hard indicator of a key paste.
	rePEM = regexp.MustCompile(`(?i)-----begin [a-z0-9 ]*private key-----`)
	// Secret-looking assignment: `password: <value>`, `api_key = <value>`, etc. The value
	// must be 6+ non-space chars right after the separator, so "password reset link: ..."
	// (a space before the value) does NOT match.
	//
	// `token` (bare) and `auth_token` were REMOVED from the keyword set: the bare word is
	// everyday prose ("token: expires_in" in OAuth docs) and matched `\S{6,}` on any
	// following word, blocking legitimate developer content (CORE-SEC-002). Real tokens
	// still trip reSecret (sk-/ghp_/JWT/…) or the hex check; what is left here are the
	// unambiguous secret-assignment keywords.
	reAssign = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|api[_ \-]?key|access[_ \-]?key|private[_ \-]?key)\s*[:=]\s*\S{6,}`)
	// Prompt injection: classic patterns plus the common variants the five fixed phrases
	// missed (prior/preceding/earlier; disregard/forget …; print/show/reveal YOUR prompt).
	// Still a heuristic, not a model — the reveal group requires "your"/"system" so a
	// benign "show me the instructions" does not trip it.
	reInjection = regexp.MustCompile(`(?i)(` +
		`(ignore|disregard|forget) (all |the |your )?(previous|above|prior|preceding|earlier|system) (instructions|rules|prompts?)` + `|` +
		`disregard the (system|above)` + `|` +
		`(reveal|print|show|expose|repeat|output|dump) (me |us )?(your (system )?|the system )(prompt|instructions|rules)` + `|` +
		`you are now [a-z]` + `|` +
		`forget (your |the )?(rules|instructions)` +
		`)`)
	// secretCtxRe is the set of keywords whose presence NEAR a long hex run turns that run
	// from "probably a hash/id" into "probably a pasted secret" (see hexLooksSecret).
	secretCtxRe = regexp.MustCompile(`(?i)(secret|password|passwd|pwd|api[_ \-]?key|access[_ \-]?key|private[_ \-]?key|token|credential|bearer|auth)`)
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

// ContainsSecret tells whether the text contains a known secret/key. It combines the
// named-token regexes, the PEM header, the assignment pattern, and a generic high-entropy
// hex check (hexLooksSecret) that — unlike the old `[0-9a-f]{40,}` catch-all — does NOT
// flag a long run of plain DIGITS (an id/number) or of repeated letters (deadbeef…) as a
// secret. Those were false positives that blocked legitimate content.
func ContainsSecret(s string) bool {
	return reSecret.MatchString(s) || rePEM.MatchString(s) || reAssign.MatchString(s) || hexLooksSecret(s)
}

// hexLooksSecret reports a contiguous run of >=32 hex characters that contains BOTH a digit
// and an a-f/A-F letter. The mixed-class requirement is what keeps a long integer (digits
// only) and a repeated-letter blob (letters only) from reading as a secret, while still
// catching a real mixed hex token/hash.
func hexLooksSecret(s string) bool {
	isHex := func(c byte) bool {
		return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
	}
	for i := 0; i < len(s); {
		if !isHex(s[i]) {
			i++
			continue
		}
		j, hasDigit, hasAlpha := i, false, false
		for j < len(s) && isHex(s[j]) {
			if s[j] >= '0' && s[j] <= '9' {
				hasDigit = true
			} else {
				hasAlpha = true
			}
			j++
		}
		run := j - i
		if run >= 32 && hasDigit && hasAlpha {
			// 32/40/64 are the lengths of an MD5/dashless-UUID, a git SHA-1 and a
			// SHA-256 — everyday developer content. Those read as a secret ONLY when a
			// secret keyword sits just before them ("secret: <64hex>"); a bare "commit
			// <40hex>" or "id <32hex>" no longer false-positives (CORE-SEC-002). Any
			// other length of mixed hex is treated as a secret as before.
			benignLen := run == 32 || run == 40 || run == 64
			lo := i - 24
			if lo < 0 {
				lo = 0
			}
			nearKeyword := secretCtxRe.MatchString(s[lo:i])
			if !benignLen || nearKeyword {
				return true
			}
		}
		i = j
	}
	return false
}

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
