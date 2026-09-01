// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Canary routing — PURE DOMAIN.
//
// The honest answer to "is this other model good enough for my flow?" is not a
// benchmark, it is the customer's own traffic. A canary sends a declared fraction
// of requests to a candidate route so cost, latency and error rate can be compared
// on real prompts, with the reference still serving everything else.
//
// What this file deliberately does NOT do is claim anything about QUALITY. We
// measure money, time and failures; response depth is not observable from here.
// The comparison surfaced to the customer says exactly that.
package routing

import (
	"crypto/sha256"
	"encoding/binary"
)

// Canary is the declared experiment for a feature.
type Canary struct {
	// Route is the candidate that receives the sampled traffic.
	Route string `json:"route"`
	// Fraction is the share of requests to sample, in (0,1]. Anything outside that
	// range disables the canary — see InCanary.
	Fraction float64 `json:"fraction"`
}

// InCanary decides whether one request belongs to the canary sample.
//
// Deterministic by request identifier, not random. Three reasons, and the first is
// the one that matters: a routing decision has to be REPRODUCIBLE. When a customer
// asks why a given request_id was served by another route, the answer must be
// recomputable from the record instead of "a coin was flipped". Randomness would
// also break the domain's purity rule (no rand, verified by boundary_test.go), and
// hashing keeps retries of the same identifier on the same side of the split.
//
// An out-of-range fraction turns the canary OFF rather than clamping. Clamping 1.5
// to 1.0 would silently send ALL traffic of a feature to a candidate route because
// of a typo — the loudest possible failure mode from the quietest possible mistake.
func InCanary(c Canary, requestID string) bool {
	if c.Route == "" || requestID == "" {
		return false
	}
	if c.Fraction <= 0 || c.Fraction > 1 {
		return false
	}
	// The route name is part of the hash input so that two features canarying
	// different candidates do not sample the exact same request identifiers, which
	// would correlate the experiments.
	sum := sha256.Sum256([]byte(c.Route + "|" + requestID))
	// 53 bits keeps the value exactly representable as a float64, so the comparison
	// below has no rounding surprises near the boundary.
	n := binary.BigEndian.Uint64(sum[:8]) >> 11
	const max = float64(uint64(1) << 53)
	return float64(n)/max < c.Fraction
}
