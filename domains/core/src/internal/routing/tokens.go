// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

// InputSplit is the partition of the input tokens across the pricing layers.
// The sum of the three parts is the request's input total, and every token is
// assigned to EXACTLY one part (Req 4.4).
type InputSplit struct {
	Uncached   int
	CacheRead  int
	CacheWrite int
}

// Total is the input token total represented by the partition.
func (s InputSplit) Total() int { return s.Uncached + s.CacheRead + s.CacheWrite }

// SplitInputTokens resolves the counting ambiguity between providers.
//
// `inclusive` comes from the model's cache_tokens_inclusive capability (data, not
// a constant): some providers already add the cached tokens into InputTokens and
// others report them separately. Getting this wrong charges twice or undercharges —
// and since the product sells an auditable ledger, that error hits exactly what we
// promise. The default assumption is EXCLUSIVE and is recorded as a risk in the design.
//
// An inconsistent provider (cacheRead+cacheWrite > reported in inclusive mode) must
// not produce a negative part: the floor is zero.
func SplitInputTokens(reported, cacheRead, cacheWrite int, inclusive bool) InputSplit {
	if reported < 0 {
		reported = 0
	}
	if cacheRead < 0 {
		cacheRead = 0
	}
	if cacheWrite < 0 {
		cacheWrite = 0
	}
	if !inclusive {
		return InputSplit{Uncached: reported, CacheRead: cacheRead, CacheWrite: cacheWrite}
	}
	uncached := reported - cacheRead - cacheWrite
	if uncached < 0 {
		uncached = 0
	}
	return InputSplit{Uncached: uncached, CacheRead: cacheRead, CacheWrite: cacheWrite}
}
