// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import (
	"math"
	"math/rand"
	"testing"
	"testing/quick"
)

func TestCosine_IdenticalIsOne(t *testing.T) {
	v := []float32{0.2, -0.5, 0.9, 0.1}
	if got := Cosine(v, v); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("Cosine(v,v) = %v, want 1.0", got)
	}
}

func TestCosine_OrthogonalIsZero(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{0, 1}
	if got := Cosine(a, b); math.Abs(got) > 1e-9 {
		t.Errorf("Cosine(orthogonal) = %v, want 0", got)
	}
}

func TestCosine_OppositeIsMinusOne(t *testing.T) {
	a := []float32{1, 2, 3}
	b := []float32{-1, -2, -3}
	if got := Cosine(a, b); math.Abs(got+1.0) > 1e-9 {
		t.Errorf("Cosine(opposite) = %v, want -1", got)
	}
}

func TestCosine_DefensiveZero(t *testing.T) {
	if Cosine(nil, nil) != 0 {
		t.Error("nil must give 0")
	}
	if Cosine([]float32{1, 2}, []float32{1}) != 0 {
		t.Error("different dimensions must give 0 (never a match, never a panic)")
	}
	if Cosine([]float32{0, 0}, []float32{1, 1}) != 0 {
		t.Error("zero magnitude must give 0")
	}
}

// Positive scaling does not change the cosine (invariance) — the basis for why
// quantizing is safe.
func TestCosine_ScaleInvariant(t *testing.T) {
	a := []float32{0.3, 0.7, -0.2}
	b := []float32{3, 7, -2} // a * 10
	if got := Cosine(a, b); math.Abs(got-1.0) > 1e-6 {
		t.Errorf("scale-invariant Cosine failed: %v", got)
	}
}

// testCtx is the context partition used by the cases that only exercise the
// similarity math. The context filter has its own tests in
// semcache_regression_test.go.
const testCtx = "ctx-teste"

func TestBestSemanticMatch_PicksHighestAboveThreshold(t *testing.T) {
	query := []float32{1, 0, 0}
	cands := []SemCandidate{
		{CacheKey: "quase", Vec: []float32{0.99, 0.14, 0}, Ctx: testCtx, Num: "-"}, // ~0.99
		{CacheKey: "longe", Vec: []float32{0, 1, 0}, Ctx: testCtx, Num: "-"},       // 0
		{CacheKey: "perfeito", Vec: []float32{1, 0, 0}, Ctx: testCtx, Num: "-"},    // 1.0
	}
	m, ok := BestSemanticMatch(query, cands, 0.9, testCtx, "-")
	if !ok || m.CacheKey != "perfeito" {
		t.Fatalf("expected a hit on 'perfeito', got %+v ok=%v", m, ok)
	}
}

func TestBestSemanticMatch_MissWhenBelowThreshold(t *testing.T) {
	query := []float32{1, 0, 0}
	cands := []SemCandidate{{CacheKey: "meia", Vec: []float32{0.7, 0.7, 0}, Ctx: testCtx, Num: "-"}} // ~0.707
	if _, ok := BestSemanticMatch(query, cands, 0.92, testCtx, "-"); ok {
		t.Error("must not HIT below the threshold (prefer a MISS over a weak match)")
	}
}

func TestBestSemanticMatch_EmptyAndDefaultThreshold(t *testing.T) {
	if _, ok := BestSemanticMatch([]float32{1}, nil, 0.9, testCtx, "-"); ok {
		t.Error("with no candidates there can be no hit")
	}
	// threshold <=0 uses the conservative default; a 0.8 similarity must NOT pass.
	cands := []SemCandidate{{CacheKey: "k", Vec: []float32{0.8, 0.6}, Ctx: testCtx, Num: "-"}} // cos with {1,0}=0.8
	if _, ok := BestSemanticMatch([]float32{1, 0}, cands, 0, testCtx, "-"); ok {
		t.Error("the default threshold (0.92) should not accept 0.8")
	}
}

// Quantizing and rebuilding preserves DIRECTION: the cosine with the original stays ~1.
func TestQuantize_PreservesDirection(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	v := make([]float32, SemDim)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	q, scale := QuantizeVec(v)
	back := DequantizeVec(q, scale)
	if s := Cosine(v, back); s < 0.999 {
		t.Errorf("quantization degraded the direction: cosine %v < 0.999", s)
	}
}

func TestQuantize_ZeroVector(t *testing.T) {
	q, scale := QuantizeVec(make([]float32, 8))
	if scale != 0 {
		t.Errorf("a null vector should have scale 0, got %v", scale)
	}
	back := DequantizeVec(q, scale)
	for _, x := range back {
		if x != 0 {
			t.Error("rebuilding a null vector should give zero")
		}
	}
}

// Property: the cosine is symmetric and always lands in [-1,1] for any pair.
func TestProperty_CosineSymmetricAndBounded(t *testing.T) {
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		n := 1 + r.Intn(300)
		a := make([]float32, n)
		b := make([]float32, n)
		for i := 0; i < n; i++ {
			a[i] = float32(r.NormFloat64())
			b[i] = float32(r.NormFloat64())
		}
		ab, ba := Cosine(a, b), Cosine(b, a)
		if math.Abs(ab-ba) > 1e-9 {
			return false
		}
		return ab >= -1.0000001 && ab <= 1.0000001
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}
