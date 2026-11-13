// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package main

import "testing"

// OBS-SEC-001: the webhook SSRF guard rejects non-https and non-routable hosts and
// accepts an ordinary public https endpoint.
func TestValidateWebhookURL(t *testing.T) {
	bad := []string{
		"http://example.com/hook",             // not https
		"https://169.254.169.254/latest/meta", // link-local (IMDS)
		"https://127.0.0.1/hook",              // loopback
		"https://10.0.0.5/hook",               // private
		"https://localhost/hook",              // localhost
		"ftp://example.com",                   // wrong scheme
		"https://",                            // no host
	}
	for _, u := range bad {
		if err := validateWebhookURL(u); err == nil {
			t.Errorf("expected %q to be rejected", u)
		}
	}
	// A public https host must pass (example.com resolves to a public address).
	if err := validateWebhookURL("https://example.com/hook"); err != nil {
		t.Errorf("expected public https webhook to pass, got %v", err)
	}
}
