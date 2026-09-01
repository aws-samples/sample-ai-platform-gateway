// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

// PROTOCOL shell: headers and HTTP response assembly. Nothing here decides
// anything — it is translation between the domain's decision and what goes on the wire.

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/aiplat/core/internal/httpapi"
)

// allowedOrigins is the allowlist of browser origins permitted to READ this
// gateway's responses. Built once at cold start from CONSOLE_ORIGIN (comma
// separated — the Contract of Environment published by the frontend domain via
// SSM), the same pattern governance/config-api uses.
//
// Deny by default, never a wildcard: a request whose Origin is not on the list
// gets NO access-control-allow-origin header at all. The browser then blocks the
// response itself, which is the correct outcome for a caller that was never
// granted access — and it removes the wildcard, which can never be combined with
// credentials and would let any page on the internet read authenticated answers.
//
// Server-to-server callers (SDKs, curl, backend services) are UNAFFECTED: CORS is
// enforced by browsers only, so the absence of the header changes nothing for a
// caller that is not a browser.
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
// lowercased, but the lookup is case-insensitive for the same reason the feature
// header lookup in handle() is: header names are case-insensitive on the wire, and
// missing the header here would CORS-block a legitimate console.
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

func corsHeaders(reqOrigin string) map[string]string {
	return allowCORS(map[string]string{
		"access-control-allow-methods": "POST,OPTIONS",
		"access-control-allow-headers": "authorization,content-type,x-aiplat-feature,x-aiplat-key",
		"access-control-max-age":       "300",
	}, reqOrigin)
}
func jsonHeaders(reqOrigin string) map[string]string {
	return allowCORS(map[string]string{"content-type": "application/json"}, reqOrigin)
}
func sseHeaders(reqOrigin string) map[string]string {
	return allowCORS(map[string]string{"content-type": "text/event-stream", "cache-control": "no-cache"}, reqOrigin)
}

type apiResp = httpapi.Response

func sresp(status int, headers map[string]string, body string) (apiResp, error) {
	return apiResp{StatusCode: status, Headers: headers, Body: body}, nil
}
func jbody(reqOrigin string, status int, obj interface{}) (apiResp, error) {
	b, _ := json.Marshal(obj)
	return sresp(status, jsonHeaders(reqOrigin), string(b))
}
func jerr(reqOrigin string, status int, msg string) (apiResp, error) {
	return jbody(reqOrigin, status, map[string]interface{}{"error": map[string]string{"message": msg}})
}
