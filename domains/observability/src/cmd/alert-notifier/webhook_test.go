// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package main

import (
	"net"
	"testing"
)

// OBS-SEC-001 / OBS-NFR-005: the delivery-time webhook SSRF guard rejects non-https and
// non-routable hosts and accepts an ordinary public https endpoint. The resolver is injected
// (lookupIP seam) so the test runs OFFLINE — no outbound DNS — and the resolve branch is
// actually exercised rather than being dead code the real net.LookupIP covers only in CI.
func TestValidateWebhookURL(t *testing.T) {
	prev := lookupIP
	defer func() { lookupIP = prev }()
	// Stub DNS: "public.example" → a public IP; "private.example" → an RFC1918 address;
	// anything else → not found. No real network.
	lookupIP = func(host string) ([]net.IP, error) {
		switch host {
		case "public.example":
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		case "private.example":
			return []net.IP{net.ParseIP("10.0.0.5")}, nil
		default:
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
	}

	bad := []string{
		"http://public.example/hook",          // not https
		"https://169.254.169.254/latest/meta", // link-local (IMDS) — literal, no DNS
		"https://127.0.0.1/hook",              // loopback — literal
		"https://10.0.0.5/hook",               // private — literal
		"https://localhost/hook",              // localhost
		"ftp://public.example",                // wrong scheme
		"https://",                            // no host
		"https://private.example/hook",        // resolves to a private IP (via the stub)
		"https://nope.example/hook",           // does not resolve (via the stub)
	}
	for _, u := range bad {
		if err := validateWebhookURL(u); err == nil {
			t.Errorf("expected %q to be rejected", u)
		}
	}
	// A host resolving to a public address passes — using the stub, not real DNS.
	if err := validateWebhookURL("https://public.example/hook"); err != nil {
		t.Errorf("expected public https webhook to pass, got %v", err)
	}
}
