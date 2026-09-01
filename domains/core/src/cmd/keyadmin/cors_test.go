// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package main

import "testing"

// This is an ADMIN API (it issues and revokes API keys): the allowlist is the only
// thing that may unlock a browser response, and a wildcard must never be emitted.
func TestKeyadminCORS_AllowlistOnly(t *testing.T) {
	saved := allowedOrigins
	allowedOrigins = map[string]bool{"https://console.example.com": true}
	t.Cleanup(func() { allowedOrigins = saved })

	h := corsHeaders("https://console.example.com")
	if h["access-control-allow-origin"] != "https://console.example.com" {
		t.Fatalf("allowlisted origin not echoed: %v", h)
	}
	if h["vary"] != "Origin" {
		t.Fatalf("vary = %q, want Origin", h["vary"])
	}

	for _, origin := range []string{"https://evil.example.com", ""} {
		h := corsHeaders(origin)
		if v, ok := h["access-control-allow-origin"]; ok {
			t.Fatalf("origin %q got access-control-allow-origin %q, want absent", origin, v)
		}
	}

	// The non-CORS headers stay untouched in every case.
	if h["content-type"] != "application/json" ||
		h["access-control-allow-methods"] != "GET,POST,DELETE,OPTIONS" ||
		h["access-control-allow-headers"] != "content-type,authorization,x-admin-token" {
		t.Fatalf("existing headers changed: %v", h)
	}
}
