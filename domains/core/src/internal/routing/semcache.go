// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Semantic cache — PURE DOMAIN (the GPTCache idea, "our way").
//
// GPTCache addresses the low hit rate of exact LLM caching by storing the EMBEDDING
// of the question and serving the response of an earlier, SEMANTICALLY close question
// (similarity search). Only the math of that decision lives here — no SDK, no
// network, no vector DB, no clock — so it is testable offline and independent of
// infrastructure (the same rule as the other pure domains).
//
// Architectural differences vs. GPTCache (project principles):
//   - No FAISS/Milvus/ONNX. The search is BRUTE-FORCE cosine over the org's
//     recent set (hundreds to a few thousand vectors) — in Go that is sub-millisecond
//     and costs ZERO infrastructure (serverless/cost-first). A dedicated vector
//     store waits for the tier that justifies it (bridge/silo).
//   - Vectors are QUANTIZED to int8 before persisting: ~4x fewer bytes, fitting
//     ~1000 entries per org in a single 400KB item of the cost/cache store. The
//     precision loss is irrelevant for the similarity threshold.
//   - Structural isolation: the caller ONLY passes candidates from the SAME org
//     (partition by org). This package never sees another org's data.
//
// Product WARNING: semantic matching admits FALSE POSITIVES (it serves the response
// of a similar, not identical, question). That is why it is opt-in, with a high and
// conservative threshold, and why the resulting savings is flagged as approximate in
// the ledger.
package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/aiplat/core/internal/ports"
)

// SemDim is the default embedding dimension (Titan v2 supports 256/512/1024; we use
// 256 for being the cheapest to store and enough for FAQ similarity).
const SemDim = 256

// SemDefaultThreshold is the cosine similarity floor for considering a HIT when the
// config does not define one. 0.92 is deliberately HIGH: we prefer a MISS (calling
// the provider) over serving a wrong response from a loose match. Adjustable per scope.
const SemDefaultThreshold = 0.92

// Cosine returns the cosine similarity between two vectors in [-1,1]. Vectors of
// different dimensions or with zero magnitude return 0 (never a match) — defensive,
// never panics. It is the only similarity metric we use.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// SemCandidate is one entry of the org's semantic index: the response cache key
// (to fetch the body later) and the embedding of the question that produced it.
//
// Ctx is the CONTEXT FINGERPRINT (system prompt + model). Without it the index would
// mix conversations from different personas and models: the same question asked of
// two different assistants has different answers, and serving one for the other is a
// correctness error, not the acceptable imprecision of an approximate cache.
type SemCandidate struct {
	CacheKey string
	Vec      []float32
	Ctx      string
	// Num is the fingerprint of the question's numbers (see NumFingerprint). It must
	// be EQUAL for a match to happen: embeddings do not distinguish 60 from 600, and
	// serving the response for one number in place of the other is an error that
	// looks correct.
	Num string
}

// SemMatch is the search result: the winning key and the score that elected it.
type SemMatch struct {
	CacheKey string
	Score    float64
}

// BestSemanticMatch scans the ORG's candidates and returns the one with the highest
// cosine similarity to the query, PROVIDED it reaches the threshold. ok=false when
// nothing passes — then the caller takes the MISS (calls the provider) and never
// serves a weak match.
//
// threshold <= 0 falls back to SemDefaultThreshold (never serve without a floor). The
// scan is linear and deterministic; ties go to the first one (stable input order).
//
// queryCtx is the context fingerprint of the current request: a candidate with a
// DIFFERENT Ctx is ignored. A candidate with an EMPTY Ctx is ignored too — those are
// entries written before this fix, whose vector was computed over the entire system
// prompt and is therefore unusable. Invalidating instead of trying to salvage them is
// the only safe path: that vector does not represent the question.
// queryNum is the fingerprint of the question's numbers: a candidate with a DIFFERENT
// set of numbers is refused before similarity is even considered. A deterministic
// guard against the failure the threshold does not catch (60 × 600 measures 0.93).
func BestSemanticMatch(query []float32, candidates []SemCandidate, threshold float64, queryCtx, queryNum string) (SemMatch, bool) {
	if threshold <= 0 {
		threshold = SemDefaultThreshold
	}
	best := SemMatch{}
	found := false
	for _, c := range candidates {
		if c.Ctx == "" || c.Ctx != queryCtx {
			continue
		}
		if c.Num != queryNum {
			continue
		}
		s := Cosine(query, c.Vec)
		if s >= threshold && s > best.Score {
			best = SemMatch{CacheKey: c.CacheKey, Score: s}
			found = true
		}
	}
	return best, found
}

// QuantizeVec compresses a float32 vector into int8 plus a scale, to persist it ~4x
// smaller. Each component becomes round(v/scale) saturated to [-127,127], where
// scale = maxAbs/127. Returns a scale of 0 for a null vector (Dequantize rebuilds zero).
//
// Why it works: cosine similarity is invariant to positive scaling, and the
// quantization error over 256 dims stays well below the threshold's margin. It costs
// far fewer bytes in DynamoDB — the "cost-first" point of the design.
func QuantizeVec(v []float32) (q []int8, scale float64) {
	maxAbs := 0.0
	for _, x := range v {
		if a := math.Abs(float64(x)); a > maxAbs {
			maxAbs = a
		}
	}
	if maxAbs == 0 {
		return make([]int8, len(v)), 0
	}
	scale = maxAbs / 127.0
	q = make([]int8, len(v))
	for i, x := range v {
		r := math.Round(float64(x) / scale)
		if r > 127 {
			r = 127
		} else if r < -127 {
			r = -127
		}
		q[i] = int8(r)
	}
	return q, scale
}

// DequantizeVec rebuilds the approximate vector from the int8 values and the scale.
// The reconstruction preserves direction (what matters for cosine), not exact
// magnitude.
func DequantizeVec(q []int8, scale float64) []float32 {
	out := make([]float32, len(q))
	if scale == 0 {
		return out
	}
	for i, x := range q {
		out[i] = float32(float64(x) * scale)
	}
	return out
}

// SemQueryText returns the text to VECTORIZE: only the USER turns, in order,
// canonicalized.
//
// Why only the user — and this is the heart of the fix:
//
// Vectorizing all messages (including the system prompt) makes the embedding
// represent the PROMPT, not the question. Measured with Titan v2 on a real app whose
// system prompt is ~930 characters and the question ~37:
//
//	                        question only   system+question
//	"maior banco" × "maior clube de futebol"      0.29            0.96
//	"maior banco" × "maior banco" (with a typo)   0.95            0.99
//
// With a threshold of 0.92 the second column turns TWO UNRELATED QUESTIONS into a
// match: the question about football received the answer about banks. The signal that
// differentiates them (the question) is diluted into noise because 96% of the text is
// constant.
//
// The system prompt is not discarded: it goes into SemContextKey and PARTITIONS the
// index. That way different personas never cross, and the similarity measures what it
// should measure.
//
// Assistant turns are left out too: they are ANSWER, not question. Including them
// would make a long conversation converge onto itself.
func SemQueryText(msgs []ports.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role != "user" || m.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(Canonicalize(m.Text))
	}
	return b.String()
}

// SemContextKey is the fingerprint of the context that must be EQUAL for two
// questions to be able to share a response: the system prompt (concatenated and
// canonicalized) and the model served.
//
// Without it, the org's index would mix different assistants: the same question
// asked of a support assistant and of a sales assistant has different answers, and the
// model changes both the content and the attributed cost.
func SemContextKey(msgs []ports.Message, model string) string {
	var b strings.Builder
	b.WriteString("m=")
	b.WriteString(model)
	b.WriteString("|s=")
	for _, m := range msgs {
		if m.Role != "system" || m.Text == "" {
			continue
		}
		b.WriteString(Canonicalize(m.Text))
		b.WriteByte('\n')
	}
	s := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(s[:])[:16] // 16 hex = 64 bits: negligible collision, small item
}

var reNum = regexp.MustCompile(`\d+(?:[.,]\d+)?`)

// NumFingerprint returns the fingerprint of the text's NUMBERS: the sorted set of
// numbers found, hashed. Text with no number returns "-".
//
// Why it exists — and this is the finding that motivated the guard:
//
// Embeddings are bad with quantity. Measured with Titan v2 on pairs that MUST be
// treated as different questions:
//
//	"o limite e de 60 requisicoes por minuto?"  ×  "...600 requisicoes..."   0.930
//	"o cache expira em 1 hora?"                 ×  "...24 horas?"            0.908
//	"meu budget e de 100 dolares?"              ×  "...1000 dolares?"        0.893
//
// The first pair CROSSES the 0.92 threshold: without this guard, someone asking about
// 60 rpm would get the answer about 600 rpm. Swapping a number in an answer is the
// worst kind of error — it looks right and is wrong.
//
// Lowering the threshold would make it worse (all three pairs get in). Raising the
// threshold would kill the legitimate hits (a real paraphrase measures 0.40–0.90).
// There is no global threshold that separates the two classes, because similarity
// drops MORE with a legitimate rewording than with a number swap. Hence a
// deterministic guard instead of moving the floor.
//
// Privacy note: we store the HASH, never the numbers nor the text. The semantic index
// still holds no prompt content.
func NumFingerprint(text string) string {
	nums := reNum.FindAllString(text, -1)
	if len(nums) == 0 {
		return "-"
	}
	norm := make([]string, 0, len(nums))
	for _, n := range nums {
		// a decimal comma and a decimal point are the same quantity for this purpose
		norm = append(norm, strings.ReplaceAll(n, ",", "."))
	}
	sort.Strings(norm) // the order in the text does not matter; the SET does
	s := sha256.Sum256([]byte(strings.Join(norm, "|")))
	return hex.EncodeToString(s[:])[:12]
}
