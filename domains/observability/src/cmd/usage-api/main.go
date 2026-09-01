// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// usage-api of the Observability domain: the unified cost correlation layer.
// It reads the Cost_Store and aggregates the spend of ALL the providers/models the
// org uses through the gateway (external providers + internal/self-hosted) into a
// single view per app/model/provider and over time. It is the input of the
// differentiator (end-to-end cost per app).
//
// After Phase 3 of the hexagonal-refactor this is a SHELL: reading the Cost_Store comes
// from the port (ports.CostStore, adapted by ddbcoststore) and the arithmetic comes from
// the pure domain (internal/telemetry). The handler only does auth/scoping, parsing and
// formatting.
//
// Routes (protected by the API Gateway JWT authorizer):
//
//	GET /usage/summary?tenant=<t>&from=<iso>&to=<iso>&bucket=day|hour
//	    → totals + breakdowns by provider, model, team, app, feature + time series + sli.
//	GET /usage/records?from&to&limit&model&result
//	    → per-request log (metadata; no prompt/response content).
package main

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiplat/observability/internal/adapters/ddbcoststore"
	"github.com/aiplat/observability/internal/ports"
	"github.com/aiplat/observability/internal/telemetry"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

var (
	costStore  ports.CostStore
	table      = os.Getenv("COST_STORE_TABLE")
	adminToken = os.Getenv("ADMIN_TOKEN")

	// A direct client for the ALERTLOG#<org> partition. It deliberately does not go
	// through ports.CostStore: that port models the Usage_Record (the usage series), and
	// the alert history is a different shape of data, with a different lifecycle.
	ddbClient *dynamodb.Client
	costTable = os.Getenv("COST_STORE_TABLE")
)

// reqPath returns the request path.
func reqPath(req events.APIGatewayProxyRequest) string {
	return req.Path
}

// claim reads one Cognito claim out of the REST API's COGNITO_USER_POOLS authorizer
// context. Claims come nested under an untyped "claims" key inside the untyped
// Authorizer map (confirmed against a real request, not assumed from docs) —
// different from the HTTP API's JWT authorizer, which nested them under a typed
// Authorizer.JWT.Claims map. A missing authorizer, "claims" key, or individual
// claim all return "", the same safe default as before.
func claim(req events.APIGatewayProxyRequest, name string) string {
	if req.RequestContext.Authorizer == nil {
		return ""
	}
	claims, ok := req.RequestContext.Authorizer["claims"].(map[string]interface{})
	if !ok {
		return ""
	}
	if v, ok := claims[name]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// getSecurityHeaders returns standard security headers for all responses
func getSecurityHeaders() map[string]string {
	return map[string]string{
		// Prevent MIME sniffing
		"X-Content-Type-Options": "nosniff",

		// Prevent clickjacking
		"X-Frame-Options": "DENY",

		// XSS Protection (legacy browsers)
		"X-XSS-Protection": "1; mode=block",

		// Content Security Policy
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",

		// HSTS - Force HTTPS for 1 year, include subdomains
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains; preload",

		// Referrer Policy - Don't leak URLs
		"Referrer-Policy": "no-referrer",

		// Permissions Policy (formerly Feature-Policy)
		"Permissions-Policy": "geolocation=(), microphone=(), camera=()",

		// Cache Control for sensitive data
		"Cache-Control": "no-store, no-cache, must-revalidate, private",
		"Pragma":        "no-cache",
		"Expires":       "0",
	}
}

// mergeHeaders merges multiple header maps (later maps override earlier ones)
func mergeHeaders(maps ...map[string]string) map[string]string {
	result := make(map[string]string)
	for _, m := range maps {
		for k, v := range m {
			result[k] = v
		}
	}
	return result
}

// allowedOrigins is the allowlist of browser origins permitted to READ this API's
// responses. Built once at cold start from CONSOLE_ORIGIN (comma separated — the
// Contract of Environment published by the frontend domain via SSM), the same
// pattern the core domain uses.
//
// Deny by default, never a wildcard: an Origin that is not on the list gets NO
// access-control-allow-origin header, so the browser blocks the response itself.
// That matters most here — this API serves the org's cost and usage data, and a
// wildcard would let any page on the internet read it (and can never be combined
// with credentials anyway). Server-to-server callers (SDKs, curl, backend services)
// are UNAFFECTED: CORS is enforced by browsers only, so the absence of the header
// changes nothing for a caller that is not a browser.
var allowedOrigins = buildAllowedOrigins()

func buildAllowedOrigins() map[string]bool {
	origins := map[string]bool{}
	for _, o := range strings.Split(os.Getenv("CONSOLE_ORIGIN"), ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			origins[o] = true
		}
	}
	return origins
}

// originOf extracts the request's Origin. API Gateway delivers the header name
// lowercased, but the lookup is case-insensitive because header names are
// case-insensitive on the wire and missing it would CORS-block a legitimate console.
func originOf(headers map[string]string) string {
	if v, ok := headers["origin"]; ok {
		return v
	}
	for k, v := range headers {
		if strings.ToLower(k) == "origin" {
			return v
		}
	}
	return ""
}

// getCORSHeaders returns CORS and security headers. It is a function of the
// REQUEST origin, not a package-level map: the caller's own origin is echoed
// (never "*") only when it is on the allowlist, together with `vary: Origin` so
// shared caches do not serve one origin's headers to another.
func getCORSHeaders(reqOrigin string) map[string]string {
	cors := map[string]string{
		"access-control-allow-methods": "GET,OPTIONS",
		"access-control-allow-headers": "content-type,authorization,x-admin-token",
		"content-type":                 "application/json",
	}
	h := mergeHeaders(cors, getSecurityHeaders())
	if reqOrigin != "" && allowedOrigins[reqOrigin] {
		h["access-control-allow-origin"] = reqOrigin
		h["vary"] = "Origin"
	}
	return h
}

// pctOf returns part/whole as a percentage, 0 when the denominator is zero.
// A zero denominator happens routinely on a new org (no requests yet).
func pctOf(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return part / whole * 100
}

func resp(reqOrigin string, status int, obj interface{}) (events.APIGatewayProxyResponse, error) {
	b, _ := json.Marshal(obj)
	return events.APIGatewayProxyResponse{StatusCode: status, Headers: getCORSHeaders(reqOrigin), Body: string(b)}, nil
}

func handle(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// Captured once and threaded through every response: CORS is decided by the
	// allowlist above (deny by default, no wildcard), so every exit path needs the
	// request's own origin to be able to echo it.
	reqOrigin := originOf(req.Headers)
	if req.HTTPMethod == "OPTIONS" {
		return events.APIGatewayProxyResponse{StatusCode: 204, Headers: getCORSHeaders(reqOrigin)}, nil
	}

	// Post-refactor: single org per deployment. Auth still uses Cognito claims for role/team/app.
	// No org_id claim needed — scoping is by team/app within the deployment.
	role := claim(req, "custom:role")
	claimTeam := claim(req, "team")
	appsClaim := claim(req, "apps")
	// Per-team enforcement: a dev/billing user bound to one team only sees
	// that team's usage. Owner/admin see the whole deployment.
	privileged := role == "owner" || role == "admin"
	teamScoped := claimTeam != "" && !privileged
	// Per-app scope: they only see their own apps' usage.
	appScoped := appsClaim != "" && !privileged
	appSet := map[string]bool{}
	for _, a := range strings.Split(appsClaim, ",") {
		if a = strings.TrimSpace(a); a != "" {
			appSet[a] = true
		}
	}

	q := req.QueryStringParameters

	// Break-glass: static admin token for initial setup (known technical debt).
	if role == "" {
		token := ""
		for k, v := range req.Headers {
			if strings.ToLower(k) == "x-admin-token" {
				token = v
			}
		}
		if token == "" || token != adminToken {
			return resp(reqOrigin, 401, map[string]string{"error": "unauthorized"})
		}
	}
	now := time.Now().UTC()

	// GET /usage/alerts → history of alert FIRINGS (not a request log).
	//
	// Post-refactor: reads ALERTLOG partition (no org prefix) — single deployment.
	//
	// Scope: an alert is a deployment-level policy (there is no per-team alert), so a
	// team-scoped user gets no slice — they are simply not the audience for this screen.
	// Only owner/admin read it.
	if p := reqPath(req); strings.HasSuffix(p, "/usage/alerts") {
		if !privileged {
			return resp(reqOrigin, 403, map[string]string{"error": "only an owner or admin can see the alert history"})
		}
		days := 30
		if d, e := strconv.Atoi(q["days"]); e == nil && d > 0 && d <= 365 {
			days = d
		}
		limit := 200
		if l, e := strconv.Atoi(q["limit"]); e == nil && l > 0 && l <= 1000 {
			limit = l
		}
		items, err := queryAlertLog(ctx, now.AddDate(0, 0, -days).Format(time.RFC3339), limit)
		if err != nil {
			return resp(reqOrigin, 500, map[string]string{"error": err.Error()})
		}
		return resp(reqOrigin, 200, map[string]interface{}{"alerts": items, "count": len(items)})
	}

	from := q["from"]
	if from == "" {
		from = now.AddDate(0, 0, -30).Format(time.RFC3339)
	}
	to := q["to"]
	if to == "" {
		to = now.Format(time.RFC3339)
	}
	bucket := q["bucket"]
	if bucket != "hour" {
		bucket = "day"
	}

	// Post-refactor: no tenant parameter — single deployment
	recs, err := costStore.Query(ctx, from, to)
	if err != nil {
		return resp(reqOrigin, 500, map[string]string{"error": err.Error()})
	}
	// The choke point of per-team enforcement: it filters the records down to the
	// claim's team BEFORE any aggregation — covering summary, records and sli at once.
	if teamScoped || appScoped {
		f := recs[:0]
		for _, r := range recs {
			if teamScoped {
				t := r.Team
				if t == "" {
					t = "default"
				}
				if t != claimTeam {
					continue
				}
			}
			if appScoped && !appSet[r.App] {
				continue
			}
			f = append(f, r)
		}
		recs = f
	}

	// GET /usage/records → per-request log (metadata; NO prompt/response content).
	// The prompt is never persisted (only its sha256, as a cache key) — see steering.
	path := req.Path
	if strings.HasSuffix(path, "/usage/records") {
		fModel := q["model"]
		fResult := q["result"] // all | served | cache | error | blocked
		// swap filter: none | same_model | equivalent | downgrade. Lets a customer
		// answer "show me every request where you served a different MODEL" without
		// scanning the whole log — the question someone auditing response quality
		// asks first. `none` selects served-as-requested.
		fSwap := q["swap"]
		limit := 200
		if l, e := strconv.Atoi(q["limit"]); e == nil && l > 0 && l <= 1000 {
			limit = l
		}
		// resultDe derives the displayed outcome from status + cache_hit.
		// An empty status = "success" (old records, always served).
		resultDe := func(r telemetry.Record) string {
			switch r.Status {
			case "blocked":
				return "blocked"
			case "error":
				return "error"
			}
			if r.CacheHit {
				return "cache"
			}
			return "served"
		}
		// most recent first
		sort.Slice(recs, func(i, j int) bool { return recs[i].TS > recs[j].TS })
		items := make([]map[string]interface{}, 0, limit)
		for _, r := range recs {
			if fModel != "" && r.Model != fModel {
				continue
			}
			result := resultDe(r)
			if fResult != "" && fResult != "all" && fResult != result {
				continue
			}
			if fSwap != "" && fSwap != "all" {
				want := fSwap
				if want == "none" {
					want = ""
				}
				if r.SwapClass != want {
					continue
				}
			}
			items = append(items, map[string]interface{}{
				"ts": r.TS, "model": r.Model, "provider": r.Provider,
				"requested_model": r.RequestedModel, "requested_cost_usd": r.RequestedCostUSD,
				"swap_class": r.SwapClass, "served_model_id": r.ServedModelID,
				"team": r.Team, "app": r.App, "feature": r.Feature,
				"tokens_in": r.TokensIn, "tokens_out": r.TokensOut,
				"cost_usd": r.Cost, "saved_usd": r.Saved, "savings_reason": r.Reason,
				"latency_ms": r.Latency, "cache_hit": r.CacheHit,
				"status": result, "result": result,
				"reason": r.FailReason, "category": r.Category, "detail": r.Detail,
				"upstream": r.Upstream,
			})
			if len(items) >= limit {
				break
			}
		}
		return resp(reqOrigin, 200, map[string]interface{}{
			"from": from, "to": to,
			"count": len(items), "records": items,
			"note": "per-request metadata; prompt/response content is never stored",
		})
	}

	// Platform reliability SLI: good / eligible. "Eligible" = a success OR a failure
	// that counts against us (provider down, platform). It excludes policy, the
	// customer's config/auth and the customer's provider quota (sli_eligible=false).
	// Only `mode: sync` enters the SLI: an asynchronous (batch) request has hours of
	// latency and would consume the error budget by design, not by failure.
	sliGood, sliDenom, failByReason, sliPct := telemetry.SLI(recs)

	// The cost summary considers only served requests: a block/error has zero cost and
	// would inflate the count/latency. Those live in the Logs tab, not here.
	recs = telemetry.Served(recs)

	total := telemetry.Totals(recs)
	// The average latency considers ONLY the synchronous path, for the same reason as
	// the SLI: mixing hours of batch with milliseconds of sync would make the average useless.
	avgLat := telemetry.AvgLatencySync(recs)
	// ROI: savings over the gross cost (the cost that would have been paid without the gateway).
	gross := total.CostUSD + total.SavedUSD
	savedPct := 0.0
	if gross > 0 {
		savedPct = total.SavedUSD / gross * 100
	}

	// Savings ledger, only for the records with attributed savings.
	var saving []telemetry.Record
	for _, r := range recs {
		if r.Saved > 0 {
			saving = append(saving, r)
		}
	}

	body := map[string]interface{}{
		"from":   from,
		"to":     to,
		"bucket": bucket,
		"totals": map[string]interface{}{
			"cost_usd":  total.CostUSD,
			"saved_usd": total.SavedUSD,
			// Savings split by STRENGTH OF PROOF. `verified` does not depend on a
			// counterfactual (cache: same model, observable avoided cost) and is the
			// defensible basis for gain-share. `counterfactual` compares against the
			// REQUESTED model — real money, but only valid if the request was a real intent.
			"saved_verified_usd":       total.SavedVer,
			"saved_counterfactual_usd": total.SavedCf,
			"saved_verified_pct":       pctOf(total.SavedVer, total.CostUSD+total.SavedVer),
			"gross_usd":                gross,
			"saved_pct":                savedPct,
			// Split of `cost_usd` by pocket. It is NOT savings: burned credit is
			// consumed balance. The ledger shows it separately precisely so proven
			// savings are not inflated with money the customer already had.
			"credit_usd": total.CreditUSD,
			"cash_usd":   total.CashUSD,
			// Provenance of the price used in the calculation. `list_price_pct` is how
			// much of the period's cost came from the provider's public table — the
			// share with systematic error if the customer has an unregistered
			// negotiated discount.
			"cost_list_price_usd":     total.CostList,
			"cost_contract_price_usd": total.CostContract,
			"list_price_pct":          pctOf(total.CostList, total.CostList+total.CostContract),
			"requests":                total.Requests,
			"tokens_in":               total.TokensIn,
			"tokens_out":              total.TokensOut,
			"cache_hits":              total.CacheHits,
			"avg_latency_ms":          avgLat,
		},
		"sli": map[string]interface{}{
			"availability_pct": sliPct,   // good / eligible (%)
			"good":             sliGood,  // served/cache requests
			"eligible":         sliDenom, // denominator (excludes policy/config/quota)
			"bad":              sliDenom - sliGood,
			"fail_by_reason":   failByReason, // breakdown of the failures by reason
		},
		"by_provider":       telemetry.Buckets(recs, func(r telemetry.Record) string { return r.Provider }),
		"by_upstream":       telemetry.Buckets(recs, func(r telemetry.Record) string { return r.Upstream }),
		"by_model":          telemetry.Buckets(recs, func(r telemetry.Record) string { return r.Model }),
		"by_team":           telemetry.Buckets(recs, func(r telemetry.Record) string { return r.Team }),
		"by_app":            telemetry.Buckets(recs, func(r telemetry.Record) string { return r.App }),
		"by_feature":        telemetry.Buckets(recs, func(r telemetry.Record) string { return r.Feature }),
		"savings_by_reason": telemetry.Buckets(saving, func(r telemetry.Record) string { return r.Reason }),
		// Breakdown by SWAP CLASS: how many requests were served as requested, how many
		// changed provider only (same model, no quality risk) and how many changed
		// model. It is the dimension `requested_model != model` does not express — that
		// comparison says a swap happened, not what the swap cost in quality.
		"by_swap_class": telemetry.BySwapClass(recs),
		"series":        telemetry.Series(recs, bucket),
	}
	// Canary comparison: it only appears when the customer asks for the feature and the
	// candidate route (`?canary_feature=&canary_route=`). Outside that, nothing — a
	// comparison without a declared experiment would be a number with no question.
	if cf, cr := q["canary_feature"], q["canary_route"]; cr != "" {
		body["canary"] = telemetry.CompareCanary(recs, cf, cr)
	}
	// Savings series by reason (cache/auto_cheapest/fallback/budget_degrade) over time —
	// the input for the per-mechanism charts plus the consolidated one in the console.
	ssLabels, ssByReason := telemetry.SavingsByReasonSeries(recs, bucket)
	body["savings_series"] = map[string]interface{}{"labels": ssLabels, "by_reason": ssByReason}
	return resp(reqOrigin, 200, body)
}

func main() {
	cfg, _ := awscfg.LoadDefaultConfig(context.TODO())
	ddbClient = dynamodb.NewFromConfig(cfg)
	costStore = ddbcoststore.New(ddbClient, table)
	lambda.Start(handle)
}
