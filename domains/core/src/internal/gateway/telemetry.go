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
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aiplat/core/internal/ports"
)

// emitUsage publishes the Usage_Record, delegating to the sqsusage adapter
// (best-effort/asynchronous; an unconfigured queue = no-op).
func emitUsage(ctx context.Context, rec map[string]interface{}) {
	usageSink.Emit(ctx, rec)
}

// usageRecordSchema versions the Usage_Record wire contract between the Core (emitter)
// and Observability (writer). Stamped on every record and asserted on both sides against
// the shared fixture at testdata/contracts/usage-record, so a renamed or dropped field
// fails a test instead of silently vanishing from the ledger (XC-ARC-001).
const usageRecordSchema = "usage-record/1"

// emitRecord stamps the schema version on a Usage_Record and emits it through the seam.
// Single choke point: every emit path (success, cache, stream, failure) goes through
// here, so none can forget the version.
func emitRecord(ctx context.Context, rec map[string]interface{}) {
	if _, ok := rec["schema"]; !ok {
		rec["schema"] = usageRecordSchema
	}
	emitUsageFn(ctx, rec)
}

// emitUsageFn is the usage EMISSION SEAM.
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

// reasonInvalidRequest is a provider 4xx caused by the CALLER (bad parameters, malformed
// body, input over the model's limit). It is classified as a client error (config, non-SLI)
// and returned to the caller as a 400 with a generic message — never as a 502 and never
// leaking the raw upstream error.
const reasonInvalidRequest = "invalid_request"

// reasonProviderConfig: the provider rejected the request because the ROUTE is
// misconfigured — an unknown/retired provider_model_id, a 404, or a ValidationException
// that the model identifier could not be resolved. CORE-BND-006: this is the OPERATOR's
// config mistake, not the caller's parameters, so it must be category config, OUT of the
// SLI, and the client message must point at the route, not tell the caller to check their
// input. Distinct from reasonInvalidRequest (a genuine bad-parameter rejection) and from
// provider_down (a healthy provider is not the same as a wrong model id).
const reasonProviderConfig = "provider_config"

// providerRouteError reports whether a provider rejection means the configured model
// ROUTE is wrong (unknown/retired model id), as opposed to a bad caller request. A 404 is
// always a route error; otherwise it matches the provider wordings for an unresolvable
// model identifier (Bedrock's "could not resolve the foundation model", "model identifier
// is invalid", end-of-life notices, and model/identifier "not found/exist/supported").
func providerRouteError(status int, body string) bool {
	if status == 404 {
		return true
	}
	b := strings.ToLower(body)
	switch {
	case strings.Contains(b, "could not resolve the foundation model"),
		strings.Contains(b, "model identifier is invalid"),
		strings.Contains(b, "provided model identifier is invalid"),
		strings.Contains(b, "end of life"), strings.Contains(b, "end-of-life"),
		strings.Contains(b, "no longer supported"):
		return true
	case (strings.Contains(b, "model") || strings.Contains(b, "identifier")) &&
		(strings.Contains(b, "not found") || strings.Contains(b, "does not exist") ||
			strings.Contains(b, "not exist") || strings.Contains(b, "not supported")):
		return true
	}
	return false
}

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
	case "invalid_body", "unknown_model", "no_eligible_model":
		return catConfig, false
	// invalid_request: the provider rejected the request as malformed / out of range /
	// too long. It is the CALLER's 4xx, not a provider outage — config category, OUT of
	// the SLI, so a client looping on bad parameters cannot burn our error budget.
	case reasonInvalidRequest:
		return catConfig, false
	// provider_config: the ROUTE names a model the provider cannot resolve (operator
	// mistake). Config category, OUT of the SLI — a wrong model id is not a platform
	// reliability failure, and a client looping on it must not burn our error budget.
	case reasonProviderConfig:
		return catConfig, false
	case "model_not_allowed":
		return catConfig, false
	// app_not_allowed: the request named an app this API key does not carry. Same shape
	// as model_not_allowed — the caller asked for something its credential does not
	// reach. It lands in `config` and OUT of the SLI: the default (dependency, eligible)
	// would count our own correct refusal as a reliability failure, so a client looping
	// on a typo would burn the error budget.
	case "app_not_allowed":
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
	// Prefer the typed status code when the adapter carried one: the number is data,
	// not text to grep. Substring matching below stays only as the fallback for errors
	// that are not ProviderError (e.g. the Bedrock SDK's ValidationException string).
	var pe *ports.ProviderError
	if errors.As(err, &pe) && pe.Status > 0 {
		b := strings.ToLower(pe.Body)
		switch {
		case pe.Status == 429:
			return "provider_rate_limited"
		case pe.Status == 401 || pe.Status == 403:
			return "provider_auth"
		case pe.Status == 402 || strings.Contains(b, "insufficient_quota") ||
			strings.Contains(b, "credit balance") || strings.Contains(b, "billing") ||
			strings.Contains(b, "quota"):
			return "provider_quota_exceeded"
		case providerRouteError(pe.Status, pe.Body):
			// CORE-BND-006: an unknown/retired model id or a 404 is the OPERATOR's route
			// mistake, not the caller's parameters. Checked before the generic 4xx so it
			// does not get the caller-blaming invalid_request treatment.
			return reasonProviderConfig
		case pe.Status >= 400 && pe.Status < 500:
			// Any other provider 4xx is the CALLER's fault (bad parameters, malformed
			// body, input too long). It must stop the chain and return 400 — never a 502
			// nor an SLI hit.
			return reasonInvalidRequest
		case pe.Status >= 500:
			return "provider_down"
		}
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
	// Provider 4xx that is the CLIENT's fault: bad parameters, malformed body, input over
	// the model's limit. The provider rejected the REQUEST, not a sign the provider is
	// unhealthy — so this must NOT be surfaced as a 502 nor counted against our SLI (that
	// is reasonInvalidRequest's job in reasonMeta). Checked AFTER auth(401/403) and
	// rate(429) so those keep their own class; the tokens below are specific to a
	// validation rejection and do not appear in a 5xx.
	// CORE-BND-006: a ValidationException (or 404 prose) that the model id cannot be
	// resolved is the operator's ROUTE mistake, not the caller's — checked BEFORE the
	// generic validationexception→invalid_request case so it is not mislabelled.
	case providerRouteError(0, e) || strings.Contains(e, "404"):
		return reasonProviderConfig
	case strings.Contains(e, "validationexception") || strings.Contains(e, "malformed input") ||
		strings.Contains(e, "http 400") || strings.Contains(e, "statuscode: 400") ||
		strings.Contains(e, "too long") || strings.Contains(e, "extraneous") ||
		strings.Contains(e, "is deprecated") || strings.Contains(e, "not permitted") ||
		strings.Contains(e, "invalid request") || strings.Contains(e, "422"):
		return reasonInvalidRequest
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
	// A failure/timeout record must survive the request's own cancellation: emitFailure
	// is reached exactly when the request ctx may already be past its deadline (a provider
	// timeout, a cut stream), and emitting on a dead ctx silently drops the one record an
	// operator needs most (CORE-NFR-002). Detach from cancellation, keep a short bound.
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	emitRecord(ectx, map[string]interface{}{
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
