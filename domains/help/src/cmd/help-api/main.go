// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// help-api of the Help domain: serves help content to the Console for two audiences.
//
//	GET /help/faq                → lists the 13 tabs (has_faq) — any authenticated user
//	GET /help/faq?tab=<k>        → public FAQ of one tab
//	GET /help/internal           → lists deep-dive topics — platform_admin ONLY
//	GET /help/internal?topic=<t> → deep-dive of one topic — platform_admin ONLY
//
// All of them accept ?lang=<en|pt|es>. Language is a USER preference (the console
// sends whatever is in localStorage), so it travels in the query string — unlike
// org and role, which come from the token and are never taken from the client. An
// invalid language falls back to the default instead of erroring: a malformed
// preference must not deny content to anyone.
//
// Security: the role comes ONLY from the JWT custom:role claim (validated by the
// API Gateway). Query string and body never elevate privilege. No internal content
// and no JWT ever reaches a log line.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"

	"github.com/aiplat/help/internal/adapters/embedstore"
	"github.com/aiplat/help/internal/help"
	"github.com/aiplat/help/internal/ports"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

var (
	store ports.ContentStore = embedstore.New()
)

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
// The internal deep-dive content served here is platform_admin-only, and a wildcard
// would let any page on the internet read the answer (and can never be combined
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
// REQUEST origin, not a constant map: the caller's own origin is echoed (never
// "*") only when it is on the allowlist, together with `vary: Origin` so shared
// caches do not serve one origin's headers to another.
func getCORSHeaders(reqOrigin string) map[string]string {
	corsHead := map[string]string{
		"access-control-allow-methods": "GET,OPTIONS",
		"access-control-allow-headers": "content-type,authorization",
		"content-type":                 "application/json",
	}
	h := mergeHeaders(corsHead, getSecurityHeaders())
	if reqOrigin != "" && allowedOrigins[reqOrigin] {
		h["access-control-allow-origin"] = reqOrigin
		h["vary"] = "Origin"
	}
	return h
}

func resp(reqOrigin string, status int, obj interface{}) (events.APIGatewayProxyResponse, error) {
	b, _ := json.Marshal(obj)
	return events.APIGatewayProxyResponse{StatusCode: status, Headers: getCORSHeaders(reqOrigin), Body: string(b)}, nil
}

// claim reads one Cognito claim out of the REST API's COGNITO_USER_POOLS authorizer
// context. Unlike the HTTP API's JWT authorizer (which nested claims under a typed
// Authorizer.JWT.Claims map), this authorizer nests them one level deeper, under an
// untyped "claims" key inside the untyped Authorizer map — confirmed against a real
// request (CloudWatch dump), not assumed from docs. A missing authorizer, "claims"
// key, or individual claim all return "", the same safe default as before.
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

func handle(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	method := req.HTTPMethod
	path := req.Path
	// Captured once and threaded through every response: CORS is decided by the
	// allowlist above (deny by default, no wildcard), so every exit path needs the
	// request's own origin to be able to echo it.
	reqOrigin := originOf(req.Headers)
	if method == "OPTIONS" {
		return events.APIGatewayProxyResponse{StatusCode: 204, Headers: getCORSHeaders(reqOrigin)}, nil
	}

	// Papel EXCLUSIVAMENTE da claim do Cognito (nunca query string/corpo).
	role := claim(req, "custom:role")

	bundle, err := store.Load(ctx)
	if err != nil {
		log.Printf(`{"lvl":"error","msg":"help catalog load failed"}`)
		return resp(reqOrigin, 500, map[string]string{"error": "help content unavailable"})
	}
	cv := bundle.ContractVersion
	q := req.QueryStringParameters
	lang := help.NormalizeLang(q["lang"])

	switch {
	// ---------- Public FAQ (any authenticated user) ----------
	case has(path, "/help/faq") && method == "GET":
		tab := q["tab"]
		if tab == "" {
			return resp(reqOrigin, 200, map[string]interface{}{
				"contract_version": cv, "lang": lang, "tabs": help.PublicListIn(bundle, lang),
			})
		}
		if !help.ValidTab(tab) {
			return resp(reqOrigin, 404, map[string]string{"error": "unknown tab"})
		}
		it, served, ok := help.FAQIn(bundle, lang, tab)
		if !ok {
			// A valid tab with no FAQ in any language: empty, NOT an error.
			return resp(reqOrigin, 200, map[string]interface{}{
				"contract_version": cv, "lang": lang, "tab": tab, "empty": true, "body": "",
			})
		}
		// served_lang != lang tells the client it fell back. Without it, a missing
		// translation is indistinguishable from one that exists.
		return resp(reqOrigin, 200, map[string]interface{}{
			"contract_version": cv, "lang": lang, "served_lang": served,
			"tab": tab, "version": it.Version, "body": it.Body,
		})

	// ---------- Internal deep-dive (platform_admin ONLY) ----------
	case has(path, "/help/internal") && method == "GET":
		if !help.CanSeeInternal(role) {
			// 403 sem revelar nada (nem identificadores).
			log.Printf(`{"lvl":"info","msg":"deep-dive denied","authorized":false}`)
			return resp(reqOrigin, 403, map[string]string{"error": "restricted access"})
		}
		topic := q["topic"]
		if topic == "" {
			log.Printf(`{"lvl":"info","msg":"deep-dive list","authorized":true}`)
			return resp(reqOrigin, 200, map[string]interface{}{
				"contract_version": cv, "lang": lang, "topics": help.InternalListIn(bundle, lang),
			})
		}
		it, served, ok := help.InternalIn(bundle, lang, topic)
		if !ok {
			log.Printf(`{"lvl":"info","msg":"deep-dive topic","authorized":true,"topic":%q,"status":404}`, topic)
			return resp(reqOrigin, 404, map[string]string{"error": "unknown topic"})
		}
		log.Printf(`{"lvl":"info","msg":"deep-dive topic","authorized":true,"topic":%q,"status":200}`, topic)
		return resp(reqOrigin, 200, map[string]interface{}{
			"contract_version": cv, "lang": lang, "served_lang": served,
			"topic": topic, "title": it.Title, "version": it.Version, "body": it.Body,
		})
	}
	return resp(reqOrigin, 404, map[string]string{"error": "not found"})
}

func has(path, suffix string) bool {
	return len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix
}

func main() { lambda.Start(handle) }
