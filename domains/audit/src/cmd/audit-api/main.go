// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// audit-api of the Audit domain: serves querying and export of the audit trail.
//
//	GET /audit/records?category&action&actor&target&from&to&limit&token
//	GET /audit/export?<same filters>   → CSV
//
// This is a SHELL: the JWT signature is validated by the NATIVE API Gateway
// authorizer (no crypto in our code); here we only read the claims, apply the
// pure domain gate and format the response.
//
// There is NO write, update or delete route. That absence is part of the
// append-only guarantee: what has no route cannot be called by mistake.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/aiplat/audit/internal/adapters/ddbtrail"
	"github.com/aiplat/audit/internal/auditcore"
	"github.com/aiplat/audit/internal/ports"
)

var trail ports.TrailStore

// exportMax is the export ceiling, the same as the existing Logs export — keeping
// the same number avoids two screens of the product having different limits for no
// reason. When it truncates, the response SAYS it truncated.
const exportMax = 1000

const defaultLimit = 100

func initAWS() {
	cfg, err := awscfg.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}
	ddb := dynamodb.NewFromConfig(cfg)
	trail = ddbtrail.New(ddb, os.Getenv("TRAIL_TABLE"))
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
// That matters most here — this API serves the audit trail, and a wildcard would
// let any page on the internet read it (and can never be combined with credentials
// anyway). Server-to-server callers (SDKs, curl, backend services) are UNAFFECTED:
// CORS is enforced by browsers only, so the absence of the header changes nothing
// for a caller that is not a browser.
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

// allowCORS echoes the REQUEST's own origin (never "*") when it is on the
// allowlist, and marks the response as varying on Origin so shared caches do not
// serve one origin's headers to another. Otherwise h is returned untouched — no
// CORS header at all (deny by default).
func allowCORS(h map[string]string, reqOrigin string) map[string]string {
	if reqOrigin != "" && allowedOrigins[reqOrigin] {
		h["access-control-allow-origin"] = reqOrigin
		h["vary"] = "Origin"
	}
	return h
}

// getCORSHeaders returns CORS and security headers. It is a function of the
// REQUEST origin, not a constant map: the allowlist above decides whether the
// origin is echoed back at all.
func getCORSHeaders(reqOrigin string) map[string]string {
	cors := map[string]string{
		"content-type":                 "application/json",
		"access-control-allow-headers": "content-type,authorization",
		"access-control-allow-methods": "GET,OPTIONS",
	}
	return allowCORS(mergeHeaders(cors, getSecurityHeaders()), reqOrigin)
}

func resp(reqOrigin string, code int, body any) events.APIGatewayProxyResponse {
	b, _ := json.Marshal(body)
	return events.APIGatewayProxyResponse{
		StatusCode: code,
		Headers:    getCORSHeaders(reqOrigin),
		Body:       string(b),
	}
}

// claims reads the Cognito claims out of the REST API's COGNITO_USER_POOLS
// authorizer context. They come nested under an untyped "claims" key inside the
// untyped Authorizer map (confirmed against a real request, not assumed from
// docs) — different from the HTTP API's JWT authorizer, which nested them under
// a typed Authorizer.JWT.Claims map. A missing authorizer or "claims" key
// returns an empty map, the same safe default as before.
func claims(req events.APIGatewayProxyRequest) map[string]string {
	out := map[string]string{}
	if req.RequestContext.Authorizer == nil {
		return out
	}
	c, ok := req.RequestContext.Authorizer["claims"].(map[string]interface{})
	if !ok {
		return out
	}
	for k, v := range c {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func handle(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	method := req.HTTPMethod
	path := req.Path
	// Captured once and threaded through every response: CORS is decided by the
	// allowlist above (deny by default, no wildcard), so every exit path needs the
	// request's own origin to be able to echo it.
	reqOrigin := originOf(req.Headers)
	if method == "OPTIONS" {
		return resp(reqOrigin, 200, map[string]string{"ok": "1"}), nil
	}

	claims := claims(req)
	role := claims["custom:role"]
	tokenOrg := claims["custom:org_id"]
	isOperator := role == auditcore.RolePlatformAdmin

	// The org ALWAYS comes from the token. Only the operator may point at another
	// org — it is the only legitimate exception, and it is explicit (?org=), never
	// implicit.
	org := tokenOrg
	if isOperator {
		if q := req.QueryStringParameters["org"]; q != "" {
			org = q
		}
	}
	if org == "" {
		return resp(reqOrigin, 400, map[string]string{"error": "org could not be determined"}), nil
	}

	if ok, why := auditcore.CanRead(role, isOperator); !ok {
		return resp(reqOrigin, 403, map[string]string{"error": "your role cannot query the audit trail (owner/admin only)", "code": why}), nil
	}

	q, errMsg := buildQuery(req, org)
	if errMsg != "" {
		return resp(reqOrigin, 400, map[string]string{"error": errMsg}), nil
	}

	switch {
	case strings.HasSuffix(path, "/audit/export"):
		return export(ctx, reqOrigin, q)
	case strings.HasSuffix(path, "/audit/records"):
		q.Limit = limitFrom(req, defaultLimit)
		q.Token = req.QueryStringParameters["token"]
		evs, next, err := trail.Query(ctx, q)
		if err != nil {
			log.Printf(`{"event":"audit_query_failed","org":%q,"error":%q}`, org, err.Error())
			return resp(reqOrigin, 502, map[string]string{"error": "failed to query the trail"}), nil
		}
		return resp(reqOrigin, 200, map[string]any{
			"records": evs, "count": len(evs), "next_token": next,
			"org": org,
		}), nil
	}
	return resp(reqOrigin, 404, map[string]string{"error": "unknown route"}), nil
}

func buildQuery(req events.APIGatewayProxyRequest, org string) (ports.TrailQuery, string) {
	p := req.QueryStringParameters
	q := ports.TrailQuery{
		Org:    org,
		Action: p["action"],
		Target: p["target"],
		Actor:  p["actor"],
		FromTS: p["from"],
		ToTS:   p["to"],
	}
	if c := p["category"]; c != "" {
		if !auditcore.ValidCategory(c) {
			return q, "invalid category"
		}
		q.Category = c
	}
	// "days" is a Console convenience (the same pattern as the Logs tab): it is
	// translated into the absolute window here, so the store only knows instants.
	if q.FromTS == "" {
		if d, err := strconv.Atoi(p["days"]); err == nil && d > 0 {
			q.FromTS = time.Now().UTC().AddDate(0, 0, -d).Format(time.RFC3339)
		}
	}
	return q, ""
}

func limitFrom(req events.APIGatewayProxyRequest, def int) int {
	if v, err := strconv.Atoi(req.QueryStringParameters["limit"]); err == nil && v > 0 {
		if v > exportMax {
			return exportMax
		}
		return v
	}
	return def
}

// export returns CSV. UTF-8 BOM at the start because Excel in Portuguese opens a
// BOM-less CSV with broken accents — the same treatment the Logs export already does.
func export(ctx context.Context, reqOrigin string, q ports.TrailQuery) (events.APIGatewayProxyResponse, error) {
	q.Limit = exportMax
	evs, next, err := trail.Query(ctx, q)
	if err != nil {
		return resp(reqOrigin, 502, map[string]string{"error": "failed to query the trail"}), nil
	}

	var sb strings.Builder
	sb.WriteString("\ufeff")
	w := csv.NewWriter(&sb)
	_ = w.Write([]string{"timestamp", "actor", "role", "actor_type", "action", "category",
		"scope", "target", "detail", "changed_fields", "truncated", "diff", "source_ip", "user_agent"})
	for _, e := range evs {
		diff := make([]string, 0, len(e.Changes))
		for _, c := range e.Changes {
			diff = append(diff, c.Path+": "+fmtVal(c.Before)+" -> "+fmtVal(c.After))
		}
		_ = w.Write([]string{
			e.TS, e.Actor.Email, e.Actor.Role, e.Actor.Type, e.Action, e.Category,
			e.Scope, e.Target, e.Detail, strconv.Itoa(e.ChangeCount), strconv.FormatBool(e.Truncated),
			strings.Join(diff, " | "), e.SourceIP, e.UserAgent,
		})
	}
	w.Flush()

	h := map[string]string{
		"content-type":        "text/csv; charset=utf-8",
		"content-disposition": `attachment; filename="auditoria.csv"`,
	}
	// Explicit truncation: an export silently cut short would make the auditor
	// conclude that is all there is.
	if next != "" {
		h["x-aiplat-truncated"] = "1"
	}
	// Merge with security headers. CORS for the CSV download follows the same
	// allowlist as every other response: the request's own origin is echoed only
	// when it is allowed, never "*".
	h = allowCORS(mergeHeaders(h, getSecurityHeaders()), reqOrigin)
	return events.APIGatewayProxyResponse{StatusCode: 200, Headers: h, Body: sb.String()}, nil
}

func fmtVal(v any) string {
	if v == nil {
		return "—"
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func main() {
	initAWS()
	lambda.Start(handle)
}
