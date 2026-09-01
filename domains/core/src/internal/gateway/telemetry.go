// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

// Gateway telemetry emission: Usage_Record, structured log and the failure taxonomy
// (reason → FCAPS category → SLI eligibility).
//
// It is shell, not domain — SLI/SLO aggregation belongs to the Observability
// domain, which reads the Cost_Store; here we only CLASSIFY, because the Core is
// the only one that knows why the provider call failed.
//
// WARNING: this file contains the protected token `AIPLAT_SLI_FAIL`, matched by a
// CloudWatch metric filter (`aiplat-poc-inf-platform-errors` in
// domains/core/tf/main.tf). Rewriting that line silently zeroes the metric and the
// system LOOKS healthier — the worst possible failure mode for a monitoring signal.
// See aiplat-language-policy.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// emitUsage publishes the Usage_Record, delegating to the sqsusage adapter
// (best-effort/asynchronous; an unconfigured queue = no-op).
func emitUsage(ctx context.Context, rec map[string]interface{}) {
	usageSink.Emit(ctx, rec)
}

// emitUsageFn is the usage EMISSION SEAM (hexagonal-refactor, task 1).
//
// Same pattern as callProviderFn: in production it points at emitUsage (SQS). The
// characterization test replaces it with an in-memory collector to capture the emitted
// Usage_Record as a golden — the safety net for the following steps. Swapping this var
// does not change behavior: the default is the real function. Every point that emits
// usage (success, cache, error, block via emitFailure) goes through here, so there is a
// single capture point.
var emitUsageFn = emitUsage

// --- Failure taxonomy (telecom FCAPS + Golden Signals) ---
// Every failure carries: reason (code), category (FCAPS), actor (blame) and whether it
// counts toward the PLATFORM's reliability SLI (only our own failures count).
const (
	catConfig     = "config"
	catAuth       = "auth"
	catPolicy     = "policy"
	catDependency = "dependency" // provider
	catPlatform   = "platform"   // us / AWS
	catCapacity   = "capacity"   // quota / budget
)

// logJSON emits a structured line on stdout → CloudWatch Logs. It is the basis for
// metric filters (cheap aggregates) and for tracing what the Core is doing.
// It NEVER logs prompt/response content — metadata only.
func logJSON(fields map[string]interface{}) {
	if fields["ts"] == nil {
		fields["ts"] = time.Now().UTC().Format(time.RFC3339)
	}
	// Correlation: carry the X-Ray trace id on every line, so a log entry found in
	// Logs Insights leads straight to the trace (and vice versa). Without it the
	// request context stops at the log line and the two systems stay disconnected.
	// Lambda sets _X_AMZN_TRACE_ID per invocation; outside Lambda it is absent and
	// the field is simply omitted.
	if fields["trace_id"] == nil {
		if tid := traceID(); tid != "" {
			fields["trace_id"] = tid
		}
	}
	b, _ := json.Marshal(fields)
	fmt.Println(string(b))
}

// traceID extracts the Root id from _X_AMZN_TRACE_ID
// ("Root=1-abc-def;Parent=...;Sampled=1" → "1-abc-def"). Returns "" when there is
// no trace (local run, or tracing disabled).
func traceID() string {
	h := os.Getenv("_X_AMZN_TRACE_ID")
	if h == "" {
		return ""
	}
	for _, part := range strings.Split(h, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(part), "Root="); ok {
			return v
		}
	}
	return ""
}

// reasonMeta is the SINGLE source of truth: it maps each reason to its category
// (FCAPS) and whether it counts toward the platform's reliability SLI. Only OUR
// failures count.
func reasonMeta(reason string) (category string, sliEligible bool) {
	switch reason {
	// customer: config / auth / body — equivalent to 4xx, not our failure
	case "invalid_body", "unknown_model":
		return catConfig, false
	case "model_not_allowed":
		return catConfig, false
	// policy / security: expected behavior, not a failure
	case "rate_limit_exceeded", "budget_exceeded", "account_suspended":
		return catPolicy, false
	// swap_not_allowed is the gateway ENFORCING the customer's declared swap
	// ceiling. Refusing on purpose is policy working, never a reliability defect —
	// counting it in the SLI would penalize us for obeying the configuration.
	case "swap_not_allowed":
		return catPolicy, false
	case "secret_detected", "prompt_injection":
		return catPolicy, false // SECURITY (a subtype of policy)
	// the customer's provider capacity (quota/balance/rate) — out of the SLI, but alerts
	case "provider_quota_exceeded", "provider_rate_limited":
		return catCapacity, false
	case "provider_auth":
		return catConfig, false // the provider credential is the customer's config
	// dependency (provider) unavailable — a real availability failure → SLI
	case "provider_unreachable", "provider_down", "provider_error":
		return catDependency, true
	// platform / AWS — our failure → SLI
	case "auth_backend_error", "platform_error":
		return catPlatform, true
	}
	return catDependency, true
}

// classifyProviderErr sub-classifies a provider failure from the error message
// (status code + keywords). Key distinction: exhausted quota/billing or invalid
// provider auth is NOT our failure; a provider that is down / times out IS.
func classifyProviderErr(err error) string {
	if err == nil {
		return "provider_error"
	}
	e := strings.ToLower(err.Error())
	switch {
	case strings.Contains(e, "insufficient_quota") || strings.Contains(e, "exceeded your current quota") ||
		strings.Contains(e, "credit balance") || strings.Contains(e, "billing") ||
		strings.Contains(e, "servicequotaexceeded") || strings.Contains(e, "quota"):
		return "provider_quota_exceeded"
	case strings.Contains(e, "401") || strings.Contains(e, "403") || strings.Contains(e, "invalid api key") ||
		strings.Contains(e, "invalid x-api-key") || strings.Contains(e, "authentication") ||
		strings.Contains(e, "accessdenied") || strings.Contains(e, "unauthorized"):
		return "provider_auth"
	case strings.Contains(e, "429") || strings.Contains(e, "too many requests") ||
		strings.Contains(e, "throttl"):
		return "provider_rate_limited"
	case strings.Contains(e, "timeout") || strings.Contains(e, "deadline") ||
		strings.Contains(e, "connection refused") || strings.Contains(e, "no such host") ||
		strings.Contains(e, "dial tcp") || strings.Contains(e, "eof"):
		return "provider_unreachable"
	case strings.Contains(e, "500") || strings.Contains(e, "502") || strings.Contains(e, "503") || strings.Contains(e, "504"):
		return "provider_down"
	}
	return "provider_error"
}

// emitFailure writes a Usage_Record for a non-served outcome (error or block), so the
// Logs tab shows refused requests — not just the served ones.
// Cost/tokens zeroed; the status distinguishes "error" (provider) from "blocked"
// (policy). Asynchronous (SQS), outside the response path. request_id comes from API
// Gateway.
// reason: a short code for the why (e.g. rate_limit_exceeded, secret_detected).
// detail: short text ONLY for a provider error (it never contains the customer's
// prompt), truncated, so the operator can diagnose without storing content.
func emitFailure(ctx context.Context, reqID string, id identity, feature, provider, model, status, reason, detail string, lat int) {
	if reqID == "" {
		reqID = strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	if len(detail) > 300 {
		detail = detail[:300]
	}
	category, sliEligible := reasonMeta(reason)
	emitUsageFn(ctx, map[string]interface{}{
		"request_id": reqID, "team": id.team, "app_tag": id.app,
		"feature": feature, "provider": provider, "model": model,
		"tokens_in": 0, "tokens_out": 0, "estimated_cost_usd": 0, "saved_usd": 0,
		"savings_reason": "", "latency_ms": lat, "cache_hit": false,
		"status": status, "reason": reason, "detail": detail,
		"category": category, "sli_eligible": sliEligible,
		"mode": "sync", // Req 6.2: on error and block too
		"ts":   time.Now().UTC().Format(time.RFC3339),
	})
	// Structured log (CloudWatch) — metadata, no content. Basis for Logs Insights.
	logJSON(map[string]interface{}{
		"lvl": "error", "evt": "gateway_request", "request_id": reqID,
		"team": id.team, "app": id.app,
		"status": status, "reason": reason, "category": category,
		"sli_eligible": sliEligible, "provider": provider, "model": model, "latency_ms": lat,
	})
	// Unique token marker for the CloudWatch metric filter (OUR failures only).
	// A bare token (no quotes) matches even if the runtime prefixes the stdout line.
	if sliEligible {
		fmt.Println("AIPLAT_SLI_FAIL reason=" + reason + " category=" + category)
	}
}
