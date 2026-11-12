// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package main

import "testing"

// GOV-SEC-005: a whole headers subtree, external_id, client_secret and any *_secret leaf
// are redacted; api_key_secret (a NAME) stays visible.
func TestAuditIsSensitive(t *testing.T) {
	redact := []string{
		"routing.m.headers.Authorization",
		"routing.m.headers.x-api-key",
		"routing.m.external_id",
		"client_secret",
		"routing.m.api_key",
		"some.password",
		"foo_secret",
	}
	for _, p := range redact {
		if !auditIsSensitive(p) {
			t.Errorf("path %q must be redacted", p)
		}
	}
	keep := []string{
		"routing.m.api_key_secret", // the NAME of a referenced secret
		"routing.m.provider",
		"budget.limit_usd",
		"routing.m.base_url",
	}
	for _, p := range keep {
		if auditIsSensitive(p) {
			t.Errorf("path %q must NOT be redacted", p)
		}
	}
}

// GOV-SEC-006: a base_url pointing at a loopback/private/link-local literal is rejected
// before the credential-bearing request leaves.
func TestValidateProviderHost(t *testing.T) {
	bad := []string{
		"https://127.0.0.1/v1", "https://localhost/v1", "https://10.0.0.5/v1",
		"https://169.254.169.254/latest/meta-data/", "https://192.168.1.1/", "https://[::1]/v1",
		"https://0.0.0.0/",
	}
	for _, u := range bad {
		if err := validateProviderHost(u); err == nil {
			t.Errorf("host %q must be rejected", u)
		}
	}
	// A public IP literal is allowed (no DNS needed in the test).
	if err := validateProviderHost("https://8.8.8.8/v1"); err != nil {
		t.Errorf("public IP must be allowed, got %v", err)
	}
}
