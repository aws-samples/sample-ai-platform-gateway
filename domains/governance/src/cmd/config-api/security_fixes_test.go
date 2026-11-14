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

// GOV-SEC-009: member `team` is charset-checked (it composes the scope key), while the
// SQL/script-style blocklist on display names is gone so legitimate names are accepted.
func TestValidateMemberTeamCharset(t *testing.T) {
	mk := func(team string) struct {
		Email    string   `json:"email"`
		Password string   `json:"password"`
		Name     string   `json:"name"`
		Role     string   `json:"role"`
		Team     string   `json:"team"`
		Apps     []string `json:"apps"`
	} {
		return struct {
			Email    string   `json:"email"`
			Password string   `json:"password"`
			Name     string   `json:"name"`
			Role     string   `json:"role"`
			Team     string   `json:"team"`
			Apps     []string `json:"apps"`
		}{Role: "dev", Team: team}
	}
	// A `#` would alias a different scope key (ORG#o#TEAM#default#APP#web) → must be rejected.
	if errs := validateMemberUpdate(mk("default#APP#web")); len(errs) == 0 {
		t.Fatalf("expected team with '#' to be rejected")
	}
	if errs := validateMemberUpdate(mk("engineering")); len(errs) != 0 {
		t.Fatalf("expected plain team id to pass, got %v", errs)
	}
}

func TestValidateTeamMetaAllowsApostrophe(t *testing.T) {
	tm := struct {
		DisplayName string `json:"display_name"`
	}{DisplayName: "O'Brien & A/B test"}
	if errs := validateTeamMeta(tm); len(errs) != 0 {
		t.Fatalf("expected legitimate display name to pass, got %v", errs)
	}
}

// GOV-SEC-008: non-admin rosters mask the local part of each e-mail.
func TestMaskEmail(t *testing.T) {
	cases := map[string]string{
		"alice@example.com": "a***@example.com",
		"b@c.io":            "b***@c.io",
		"noatsign":          "***",
		"":                  "***",
	}
	for in, want := range cases {
		if got := maskEmail(in); got != want {
			t.Errorf("maskEmail(%q)=%q want %q", in, got, want)
		}
	}
}

// GOV-UX-002: overriding a model that exists in a parent scope is a model_update,
// even though the child scope had no prior value (before==nil).
func TestAuditClassifyModelScoped(t *testing.T) {
	addAtChild := []auditChange{{Path: "routing.claude-haiku.provider_model_id", After: "x"}}
	if got := auditClassifyModelScoped(addAtChild, true); got != audModelUpdate {
		t.Errorf("override of parent model: got %q want %q", got, audModelUpdate)
	}
	// With no parent alias, it is a genuine add.
	if got := auditClassifyModelScoped(addAtChild, false); got != audModelAdd {
		t.Errorf("new model: got %q want %q", got, audModelAdd)
	}
}

// OBS-SEC-001: config-api rejects an SSRF alert webhook at write time and accepts a
// public https one; webhookURLFromConfig only fires when the write sets it.
func TestValidateWebhookURL_ConfigAPI(t *testing.T) {
	for _, u := range []string{
		"http://example.com/hook",
		"https://169.254.169.254/latest/meta-data/",
		"https://127.0.0.1/x",
		"https://10.1.2.3/x",
		"https://localhost/x",
	} {
		if err := validateWebhookURL(u); err == nil {
			t.Errorf("expected %q rejected", u)
		}
	}
	if err := validateWebhookURL("https://example.com/hook"); err != nil {
		t.Errorf("expected public https to pass, got %v", err)
	}
	// OBS-NFR-005: write-time validation does NOT resolve DNS, so a syntactically valid
	// https host that does not resolve is ACCEPTED at save time (the SSRF resolve check runs
	// at delivery in the notifier). A host that stops resolving must not block saving the
	// Alerts tab, and the test must not depend on outbound DNS.
	if err := validateWebhookURL("https://nonexistent-host-xyz.invalid/hook"); err != nil {
		t.Errorf("an unresolvable but syntactically valid https host must be accepted at write time, got %v", err)
	}
	// A literal private IP is still rejected without any lookup.
	if err := validateWebhookURL("https://192.168.0.5/hook"); err == nil {
		t.Errorf("a literal private IP must be rejected at write time")
	}
	if got := webhookURLFromConfig(map[string]interface{}{"budget": 1}); got != "" {
		t.Errorf("no alerts block should yield empty, got %q", got)
	}
	cfg := map[string]interface{}{"alerts": map[string]interface{}{"webhook_url": "https://example.com/h"}}
	if got := webhookURLFromConfig(cfg); got != "https://example.com/h" {
		t.Errorf("expected webhook extracted, got %q", got)
	}
}
