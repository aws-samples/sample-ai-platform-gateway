// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"os"
	"testing"
)

// The CORS contract of this gateway: echo the request's own origin when it is on
// the allowlist, emit NOTHING otherwise, and never a wildcard.
func TestCORS_AllowlistOnly(t *testing.T) {
	saved := allowedOrigins
	allowedOrigins = map[string]bool{"https://console.example.com": true}
	t.Cleanup(func() { allowedOrigins = saved })

	cases := []struct {
		name   string
		origin string
		want   string // "" = header must be absent
	}{
		{"allowlisted origin is echoed", "https://console.example.com", "https://console.example.com"},
		{"unknown origin is denied", "https://evil.example.com", ""},
		{"no origin header (server-to-server)", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, h := range []map[string]string{
				corsHeaders(tc.origin), jsonHeaders(tc.origin), sseHeaders(tc.origin),
			} {
				got, ok := h["access-control-allow-origin"]
				if got == "*" {
					t.Fatalf("wildcard emitted: %v", h)
				}
				if tc.want == "" {
					if ok {
						t.Fatalf("expected no access-control-allow-origin, got %q", got)
					}
					if _, hasVary := h["vary"]; hasVary {
						t.Fatalf("expected no vary header when denying, got %v", h)
					}
					continue
				}
				if got != tc.want {
					t.Fatalf("access-control-allow-origin = %q, want %q", got, tc.want)
				}
				if h["vary"] != "Origin" {
					t.Fatalf("vary = %q, want Origin", h["vary"])
				}
			}
		})
	}
}

// The allowlist comes from CONSOLE_ORIGIN: comma separated, trimmed, empties skipped.
func TestBuildAllowedOrigins(t *testing.T) {
	t.Setenv("CONSOLE_ORIGIN", " https://a.example.com , ,https://b.example.com ")
	got := buildAllowedOrigins()
	if !got["https://a.example.com"] || !got["https://b.example.com"] || len(got) != 2 {
		t.Fatalf("unexpected allowlist: %v", got)
	}

	os.Unsetenv("CONSOLE_ORIGIN")
	if len(buildAllowedOrigins()) != 0 {
		t.Fatal("no CONSOLE_ORIGIN must yield an empty allowlist (deny by default)")
	}
}

// Header names arrive lowercased from API Gateway, but the lookup tolerates any
// casing: missing the Origin would CORS-block a legitimate console.
func TestOriginOf(t *testing.T) {
	if got := originOf(map[string]string{"origin": "https://a"}); got != "https://a" {
		t.Fatalf("got %q", got)
	}
	if got := originOf(map[string]string{"Origin": "https://b"}); got != "https://b" {
		t.Fatalf("got %q", got)
	}
	if got := originOf(map[string]string{"authorization": "Bearer x"}); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
