// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package reconcile extracts the gateway-side TRUTH from the AgentCore Gateway's vended
// routing logs: which target/model actually served a request, and how many outbound
// attempts (gateway invocations) it took.
//
// # Why this is a GATEWAY-FACT recorder, not a per-row reconciler
//
// MEASURED 2026-10-26: the gateway's vended APPLICATION_LOGS carry only the gateway's OWN
// request_id / trace_id / span_id — a caller-supplied correlation header (AIPlat sends
// X-Aiplat-Request-Id) does NOT appear in the log. So there is no key to join a gateway
// log line back to a specific AIPlat ledger row. A gateway_fallback row therefore stays
// honestly marked reconcile_state="pending" on the AIPlat side (it cannot be auto-flipped
// per row), and THIS package instead records the gateway's own facts keyed by the gateway
// request id, so the served-target distribution and the per-attempt invocation count are
// captured for accounting even though the per-row stitch is not possible.
//
// The parsing is PURE (no IO) so the whole extraction — which lines are routing records,
// how the served fallback hop maps to an attempt count — is unit-testable without a live
// log stream.
package reconcile

import (
	"encoding/json"
	"strconv"
	"strings"
)

// RoutingFact is the gateway-side truth extracted from one vended routing-log record.
type RoutingFact struct {
	// GatewayRequestID is the gateway's own request id (its join key, not AIPlat's).
	GatewayRequestID string
	RequestedModel   string
	ResolvedModel    string
	TargetName       string
	// ServedHop is the fallback hop that served: 0 = the primary served (no fallback),
	// N = the Nth TargetFallback entry served. Parsed from the routing reason
	// "servedViaFallbackHop:N".
	ServedHop int
	// Invocations is the number of OUTBOUND attempts the gateway made, each billed as one
	// gateway invocation: the primary plus every fallback hop up to the one that served =
	// ServedHop + 1. (The circuit breaker can skip a known-failing primary; this is the
	// count derivable from the served-hop alone, which is the honest lower bound the log
	// supports without the per-attempt lines.)
	Invocations int
	// EventTS is the log event timestamp in epoch milliseconds.
	EventTS int64
	// Fallback is true when the request was served by a fallback hop rather than the
	// primary.
	Fallback bool
}

// logLine mirrors the vended routing-log JSON. Only the fields this package reads are
// declared; unknown fields are ignored.
type logLine struct {
	EventTimestamp int64  `json:"event_timestamp"`
	RequestID      string `json:"request_id"`
	Body           struct {
		ID      string `json:"id"`
		Routing *struct {
			RequestedModelID string `json:"requestedModelId"`
			ResolvedModelID  string `json:"resolvedModelId"`
			TargetName       string `json:"targetName"`
			Reason           string `json:"reason"`
		} `json:"routing"`
	} `json:"body"`
}

// ParseLogLine extracts a RoutingFact from one vended-log line. ok is false when the line
// is not a routing record (the gateway also logs "Action resolved…", "Executing Http
// request…" and other non-routing lines, which carry no served-target decision).
func ParseLogLine(line []byte) (RoutingFact, bool) {
	var l logLine
	if json.Unmarshal(line, &l) != nil {
		return RoutingFact{}, false
	}
	r := l.Body.Routing
	if r == nil || r.ResolvedModelID == "" {
		return RoutingFact{}, false
	}
	id := l.RequestID
	if id == "" {
		id = l.Body.ID
	}
	hop := parseServedHop(r.Reason)
	return RoutingFact{
		GatewayRequestID: id,
		RequestedModel:   r.RequestedModelID,
		ResolvedModel:    r.ResolvedModelID,
		TargetName:       r.TargetName,
		ServedHop:        hop,
		Invocations:      hop + 1,
		EventTS:          l.EventTimestamp,
		Fallback:         hop > 0,
	}, true
}

// parseServedHop reads the hop number out of a routing reason. The measured forms are
// "servedViaFallbackHop:1" (served by the first fallback) and, for a primary-served
// request, a reason that does not name a hop (e.g. "Model routing succeeded" or empty) →
// hop 0. Anything unparseable is treated as hop 0 (primary), which never over-counts
// invocations.
func parseServedHop(reason string) int {
	const key = "servedViaFallbackHop:"
	i := strings.Index(reason, key)
	if i < 0 {
		return 0
	}
	rest := reason[i+len(key):]
	// take the leading digits
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil || n < 0 {
		return 0
	}
	return n
}
