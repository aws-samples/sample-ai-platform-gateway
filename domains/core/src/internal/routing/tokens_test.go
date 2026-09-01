// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import (
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
)

func TestSplitInputTokens(t *testing.T) {
	tests := []struct {
		name                            string
		reported, cacheRead, cacheWrite int
		inclusive                       bool
		want                            InputSplit
	}{
		{
			name:     "exclusive: reported does not include the cached ones",
			reported: 1000, cacheRead: 800, cacheWrite: 0, inclusive: false,
			want: InputSplit{Uncached: 1000, CacheRead: 800, CacheWrite: 0},
		},
		{
			name:     "inclusive: reported already includes the cached ones",
			reported: 1000, cacheRead: 800, cacheWrite: 0, inclusive: true,
			want: InputSplit{Uncached: 200, CacheRead: 800, CacheWrite: 0},
		},
		{
			name:     "inclusive with cache write",
			reported: 1000, cacheRead: 300, cacheWrite: 200, inclusive: true,
			want: InputSplit{Uncached: 500, CacheRead: 300, CacheWrite: 200},
		},
		{
			// An inconsistent provider must not produce a negative part: it would
			// undercharge and the ledger would become irreconcilable.
			name:     "inconsistent inclusive: floor of zero instead of negative",
			reported: 100, cacheRead: 500, cacheWrite: 0, inclusive: true,
			want: InputSplit{Uncached: 0, CacheRead: 500, CacheWrite: 0},
		},
		{
			name:     "no cache at all",
			reported: 1234, inclusive: false,
			want: InputSplit{Uncached: 1234},
		},
		{
			name:     "negative inputs become zero",
			reported: -5, cacheRead: -3, cacheWrite: -1, inclusive: false,
			want: InputSplit{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitInputTokens(tc.reported, tc.cacheRead, tc.cacheWrite, tc.inclusive)
			if got != tc.want {
				t.Errorf("SplitInputTokens() = %+v, expected %+v", got, tc.want)
			}
		})
	}
}

// --- Property 5: every input token is charged exactly once (Req 4.4) ----------

type genSplit struct {
	reported, cacheRead, cacheWrite int
	inclusive                       bool
}

func (genSplit) Generate(r *rand.Rand, _ int) reflect.Value {
	g := genSplit{
		reported:   r.Intn(100_000),
		cacheRead:  r.Intn(100_000), // may exceed reported ON PURPOSE
		cacheWrite: r.Intn(10_000),
		inclusive:  r.Intn(2) == 0,
	}
	return reflect.ValueOf(g)
}

func TestProperty5_ParticaoDeTokens(t *testing.T) {
	f := func(g genSplit) bool {
		s := SplitInputTokens(g.reported, g.cacheRead, g.cacheWrite, g.inclusive)

		// No negative part — charging a negative would be an undue credit.
		if s.Uncached < 0 || s.CacheRead < 0 || s.CacheWrite < 0 {
			return false
		}
		// The cache parts are preserved: they are facts reported by the provider.
		if s.CacheRead != g.cacheRead || s.CacheWrite != g.cacheWrite {
			return false
		}
		if g.inclusive {
			// In inclusive mode the total reproduces the reported value, except when
			// the provider was incoherent and the zero floor kicked in.
			if s.Total() == g.reported {
				return true
			}
			return s.Uncached == 0 && g.cacheRead+g.cacheWrite > g.reported
		}
		// In exclusive mode the total is the reported value plus the cached ones.
		return s.Total() == g.reported+g.cacheRead+g.cacheWrite
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}
