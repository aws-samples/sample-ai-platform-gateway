// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

// Per-request app attribution: one API key, several projects (issue #3).
//
// What these tests defend is not the happy path — it is the refusal. `app` is the last
// link of the config scope chain, so it selects the budget, the rate limit, the allowed
// model list and the guardrails. An app accepted without checking the key would let a
// caller charge its spend to another project's budget and inherit its limits. A bug here
// does not look like a bug: every request still succeeds, the numbers are just attributed
// to the wrong place.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aiplat/core/internal/httpapi"
)

func TestResolveAppPrecedence(t *testing.T) {
	id := identity{team: "eng", app: "web", apps: []string{"api", "batch"}}

	cases := []struct {
		name    string
		headers map[string]string
		body    string
		want    string
		refused string
	}{
		{"nothing named falls back to the key's app", nil, "", "web", ""},
		{"header names an allowed app", map[string]string{"x-aiplat-app": "api"}, "", "api", ""},
		{"header casing is irrelevant", map[string]string{"X-AIPlat-App": "batch"}, "", "batch", ""},
		{"body field works for callers that cannot set headers", nil, "batch", "batch", ""},
		{"header wins over body", map[string]string{"x-aiplat-app": "api"}, "batch", "api", ""},
		{"whitespace is trimmed", map[string]string{"x-aiplat-app": "  api  "}, "", "api", ""},
		{"empty header falls through to the body", map[string]string{"x-aiplat-app": ""}, "api", "api", ""},
		{"the key's own app is always allowed", map[string]string{"x-aiplat-app": "web"}, "", "web", ""},
		{"an app the key does not carry is refused", map[string]string{"x-aiplat-app": "finance"}, "", "web", "finance"},
		{"case differences are NOT the same app", map[string]string{"x-aiplat-app": "API"}, "", "web", "API"},
		{"body can be refused too", nil, "finance", "web", "finance"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, refused := resolveApp(id, c.headers, c.body)
			if got != c.want || refused != c.refused {
				t.Errorf("resolveApp = (%q, %q), want (%q, %q)", got, refused, c.want, c.refused)
			}
		})
	}
}

// A single-app key (every key issued before this feature) must behave exactly as before:
// naming its own app works, naming anything else is refused.
func TestResolveAppSingleAppKeyUnchanged(t *testing.T) {
	id := identity{team: "eng", app: "web"}
	if got, refused := resolveApp(id, nil, ""); got != "web" || refused != "" {
		t.Errorf("no app named: got (%q,%q), want (web,)", got, refused)
	}
	if _, refused := resolveApp(id, map[string]string{"x-aiplat-app": "other"}, ""); refused != "other" {
		t.Errorf("a single-app key must refuse another app, got refused=%q", refused)
	}
}

// The 403 has to name the valid values. Without them a typo becomes a support question,
// and the caller cannot tell "not allowed" from "does not exist".
func TestAllowedAppsListsEveryReachableApp(t *testing.T) {
	got := allowedApps(identity{app: "web", apps: []string{"api", "web", "batch"}})
	if got != "web, api, batch" {
		t.Errorf("allowedApps = %q; expected the default first and no duplicate of it", got)
	}
	if got := allowedApps(identity{app: "web"}); got != "web" {
		t.Errorf("single-app key: allowedApps = %q, want web", got)
	}
}

// End to end: the app named on the request is what lands in the usage record, which is the
// whole point — that is the number a per-project cost report is built from.
func TestRequestAppReachesTheUsageRecord(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	recs := installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef, _ invocation) (result, error) {
		return result{text: "ok", tin: 4, tout: 2}, nil
	})
	// fakeAuth resolves app "apptest"; widen it so "proj-b" is reachable.
	prev := authResolveFn
	authResolveFn = func(context.Context, map[string]string) (identity, bool, error) {
		return identity{team: "default", app: "apptest", apps: []string{"proj-b"}}, true, nil
	}
	t.Cleanup(func() { authResolveFn = prev })

	resp, err := handle(context.Background(), httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers: map[string]string{"authorization": "Bearer test", "x-aiplat-app": "proj-b"},
		Body:    `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, RequestID: "test-app-attr",
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, expected 200", resp.StatusCode)
	}
	if len(*recs) != 1 {
		t.Fatalf("expected 1 Usage_Record, got %d", len(*recs))
	}
	if got := (*recs)[0]["app_tag"]; got != "proj-b" {
		t.Errorf("app_tag = %v, expected proj-b (the app named on the request)", got)
	}
	// The client is told which app it was charged to, so a wrapper can surface it
	// without keeping its own bookkeeping.
	var out map[string]interface{}
	json.Unmarshal([]byte(resp.Body), &out)
	meta, _ := out["aiplat"].(map[string]interface{})
	if meta == nil || meta["app_tag"] != "proj-b" {
		t.Errorf("response aiplat.app_tag = %v, expected proj-b", meta["app_tag"])
	}
}

// A refused app must be a 403 that names the problem, and must NOT be served. If it were
// served under the key's default app, the caller would believe project B was being
// measured while every request landed on project A.
func TestRequestUnknownAppIsRefused(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	providerCalled := false
	recs := installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef, _ invocation) (result, error) {
		providerCalled = true
		return result{text: "ok"}, nil
	})

	resp, err := handle(context.Background(), httpapi.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers: map[string]string{"authorization": "Bearer test", "x-aiplat-app": "someone-elses-app"},
		Body:    `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, RequestID: "test-app-refused",
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 403 {
		t.Fatalf("status = %d, expected 403", resp.StatusCode)
	}
	if providerCalled {
		t.Error("a refused app must not reach the provider")
	}
	if !strings.Contains(resp.Body, "someone-elses-app") || !strings.Contains(resp.Body, "apptest") {
		t.Errorf("the error should name the refused app and the allowed ones: %s", resp.Body)
	}
	// The refusal is recorded like every other block, so it is visible in the log
	// instead of being a silent 403.
	if len(*recs) != 1 {
		t.Fatalf("expected the block to emit 1 record, got %d", len(*recs))
	}
	if got := (*recs)[0]["reason"]; got != "app_not_allowed" {
		t.Errorf("reason = %v, expected app_not_allowed", got)
	}
	// Classification matters as much as the refusal: the default for an unknown reason is
	// (dependency, sli_eligible=true), which would count our own correct refusal against
	// the reliability SLI. A client looping on a typo would then burn the error budget.
	if got := (*recs)[0]["category"]; got != "config" {
		t.Errorf("category = %v, expected config (the caller's credential does not reach that app)", got)
	}
	if got := (*recs)[0]["sli_eligible"]; got != false {
		t.Errorf("sli_eligible = %v, expected false — refusing on purpose is not a reliability failure", got)
	}
}
