// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// CHARACTERIZATION test for config-api (hexagonal-refactor, task 9).
//
// Captures the CURRENT behavior of the Governance domain's decision rules BEFORE
// any move into internal/govcore. If, after the migration, the characterization
// reports a difference, that difference is a refactor defect (D6) — you do not
// update the golden, you fix the code.
//
// Covers:
//   - the scope chain (scopeKey/scopeKeys) and the deep merge (deepMerge);
//   - the role matrix as a truth table (resolveAccess × forceOrgFor × effTeamFor);
//   - plan rules (seatsFor, teamTier);
//   - status codes and error format of the admin routes on the paths that return
//     BEFORE any AWS call (no network, no credentials).
//
// Runs offline: package main (same package), without touching
// DynamoDB/Cognito/Secrets.
package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// ---- Scope chain + deep merge ----

func TestChar_ScopeKey(t *testing.T) {
	cases := []struct {
		name, org, team, app, want string
	}{
		{"global", "", "", "", "global"},
		{"org alone", "acme", "", "", "ORG#acme"},
		{"org+team", "acme", "sre", "", "ORG#acme#TEAM#sre"},
		{"org+team+app", "acme", "sre", "api", "ORG#acme#TEAM#sre#APP#api"},
		{"team absent with app (default)", "acme", "", "api", "ORG#acme#TEAM#default#APP#api"},
	}
	for _, c := range cases {
		if got := scopeKey(c.org, c.team, c.app); got != c.want {
			t.Errorf("%s: scopeKey(%q,%q,%q)=%q, want %q", c.name, c.org, c.team, c.app, got, c.want)
		}
	}
}

func TestChar_ScopeKeys(t *testing.T) {
	cases := []struct {
		name, org, team, app string
		want                 []string
	}{
		{"global", "", "", "", []string{"global"}},
		// INTENTIONAL change (not a regression): the merge chain for org-without-team
		// now includes TEAM#default, aligning with the Core (what the gateway
		// applies). The write target is still ORG#acme — see TestChar_ScopeKey.
		{"org alone", "acme", "", "", []string{"global", "ORG#acme", "ORG#acme#TEAM#default"}},
		{"org+team", "acme", "sre", "", []string{"global", "ORG#acme", "ORG#acme#TEAM#sre"}},
		{"org+team+app", "acme", "sre", "api", []string{"global", "ORG#acme", "ORG#acme#TEAM#sre", "ORG#acme#TEAM#sre#APP#api"}},
		{"team absent with app", "acme", "", "api", []string{"global", "ORG#acme", "ORG#acme#TEAM#default", "ORG#acme#TEAM#default#APP#api"}},
	}
	for _, c := range cases {
		if got := scopeKeys(c.org, c.team, c.app); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: scopeKeys(%q,%q,%q)=%v, want %v", c.name, c.org, c.team, c.app, got, c.want)
		}
	}
}

func TestChar_DeepMerge(t *testing.T) {
	// Maps merge by key; scalars and lists replace.
	dst := map[string]interface{}{
		"cache_ttl": 15,
		"routing":   map[string]interface{}{"auto_cheapest": true, "models": []interface{}{"a"}},
		"budget":    map[string]interface{}{"limit_usd": float64(5)},
	}
	src := map[string]interface{}{
		"cache_ttl": 60,                                                   // scalar replaces
		"routing":   map[string]interface{}{"models": []interface{}{"b"}}, // list replaces, auto_cheapest preserved
		"plan":      "pro",                                                // new key
	}
	deepMerge(dst, src)

	want := map[string]interface{}{
		"cache_ttl": 60,
		"routing":   map[string]interface{}{"auto_cheapest": true, "models": []interface{}{"b"}},
		"budget":    map[string]interface{}{"limit_usd": float64(5)},
		"plan":      "pro",
	}
	if !reflect.DeepEqual(dst, want) {
		t.Errorf("deepMerge diverged:\n got=%#v\nwant=%#v", dst, want)
	}
}

// ---- Role matrix (truth table) ----
//
// For each (role × team) it captures isPlatform/canAdmin/teamScoped and the
// result of forceOrgFor/effTeamFor. This is where a mistake becomes privilege
// escalation.

func TestChar_RoleMatrix(t *testing.T) {
	const claimOrg = "acme"
	type want struct {
		isPlatform, canAdmin, teamScoped bool
	}
	cases := []struct {
		role, team string
		w          want
	}{
		{"platform_admin", "", want{true, true, false}},
		{"platform_admin", "sre", want{true, true, false}}, // platform is never team-scoped
		{"owner", "", want{false, true, false}},
		{"owner", "sre", want{false, true, false}}, // owner sees the whole org even with a team claim
		{"admin", "", want{false, true, false}},
		{"admin", "sre", want{false, true, false}},
		{"billing", "", want{false, false, false}},
		{"billing", "sre", want{false, false, true}}, // billing with a team → locked to the team
		{"dev", "", want{false, false, false}},
		{"dev", "sre", want{false, false, true}},
		{"", "", want{false, false, false}},             // role absent
		{"", "sre", want{false, false, true}},           // role absent + team → team-scoped
		{"unknown_role", "", want{false, false, false}}, // an unknown role never administers
		{"unknown_role", "sre", want{false, false, true}},
	}
	for _, c := range cases {
		acc := resolveAccess(claimOrg, c.role, c.team)
		got := want{acc.isPlatform, acc.canAdmin, acc.teamScoped}
		if got != c.w {
			t.Errorf("resolveAccess(role=%q,team=%q)=%+v, want %+v", c.role, c.team, got, c.w)
		}
	}
}

func TestChar_ForceOrg(t *testing.T) {
	cases := []struct {
		name, role, claimOrg, param string
		wantOrg                     string
		wantOK                      bool
	}{
		{"platform without param → global", "platform_admin", "", "", "", true},
		{"platform honors the param", "platform_admin", "", "other", "other", true},
		{"platform with an org in the token still honors the param", "platform_admin", "acme", "other", "other", true},
		{"owner is forced to its own org", "owner", "acme", "other", "acme", true},
		{"dev is forced to its own org", "dev", "acme", "other", "acme", true},
		{"user without an org in the token → refused", "owner", "", "acme", "", false},
	}
	for _, c := range cases {
		acc := resolveAccess(c.claimOrg, c.role, "")
		gotOrg, gotOK := forceOrgFor(acc, c.claimOrg, c.param)
		if gotOrg != c.wantOrg || gotOK != c.wantOK {
			t.Errorf("%s: forceOrgFor=(%q,%v), want (%q,%v)", c.name, gotOrg, gotOK, c.wantOrg, c.wantOK)
		}
	}
}

func TestChar_EffTeam(t *testing.T) {
	cases := []struct {
		name, role, claimTeam, param, want string
	}{
		{"team-scoped ignores the param, uses the claim", "dev", "sre", "outro", "sre"},
		{"not team-scoped honors the param", "owner", "sre", "outro", "outro"},
		{"not team-scoped without a param", "admin", "", "", ""},
		{"platform honors the param", "platform_admin", "sre", "outro", "outro"},
	}
	for _, c := range cases {
		acc := resolveAccess("acme", c.role, c.claimTeam)
		if got := effTeamFor(acc, c.claimTeam, c.param); got != c.want {
			t.Errorf("%s: effTeamFor=%q, want %q", c.name, got, c.want)
		}
	}
}

// Plan-tier rules (seatsFor/teamTier) were removed with the SaaS billing model:
// single-client deployments have no seat ceiling and no plan-mandated team
// requirement — a team is always optional.

// ---- Routes: status and error format (paths without AWS) ----
//
// Only scenarios that return BEFORE any AWS call. That keeps the test offline and
// deterministic while still capturing the routes' error contract (R12.5).

// jwtReq builds a REST API proxy event. The COGNITO_USER_POOLS authorizer nests
// claims under an untyped "claims" key inside the untyped Authorizer map (see
// the "claim"/"allClaims" helpers in main.go) — that shape, not the old typed
// JWT.Claims map, is what a real request actually carries.
func jwtReq(method, path, org, role, team, body string, q map[string]string) events.APIGatewayProxyRequest {
	r := events.APIGatewayProxyRequest{Body: body, QueryStringParameters: q}
	r.HTTPMethod = method
	r.Path = path
	r.RequestContext.Authorizer = map[string]interface{}{
		"claims": map[string]interface{}{"custom:org_id": org, "custom:role": role, "team": team},
	}
	return r
}

func TestChar_RouteErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		req        events.APIGatewayProxyRequest
		wantStatus int
		wantErrSub string // substring expected in the "error" field (empty = not checked)
	}{
		{
			"OPTIONS → 204 with no body",
			jwtReq("OPTIONS", "/admin/config", "acme", "owner", "", "", nil),
			204, "",
		},
		{
			"GET config without an org in the token (not platform) → 403",
			jwtReq("GET", "/admin/config", "", "owner", "", "", nil),
			403, "org could not be determined from the token",
		},
		{
			"PUT config with the dev role → 403",
			jwtReq("PUT", "/admin/config", "acme", "dev", "", "{}", nil),
			403, "owner/admin only",
		},
		{
			"PUT config as admin with invalid JSON → 400",
			jwtReq("PUT", "/admin/config", "acme", "admin", "", "{invalid", nil),
			400, "invalid JSON",
		},
		{
			"GET credits with the dev role → 403",
			jwtReq("GET", "/admin/credits", "acme", "dev", "", "", nil),
			403, "cannot view the org credit",
		},
		{
			"PUT credits with the billing role → 403 (owner/admin only can change it)",
			jwtReq("PUT", "/admin/credits", "acme", "billing", "", "{}", nil),
			403, "owner/admin only",
		},
		{
			"POST secrets with the dev role → 403",
			jwtReq("POST", "/admin/secrets", "acme", "dev", "", "{}", nil),
			403, "cannot store credentials",
		},
		{
			"POST members with invalid JSON → 400",
			jwtReq("POST", "/admin/members", "acme", "owner", "", "{bad", nil),
			400, "invalid JSON",
		},
		{
			"GET members without an org in the token → 403",
			jwtReq("GET", "/admin/members", "", "owner", "", "", nil),
			403, "org could not be determined from the token",
		},
		{
			"GET bedrock models with an invalid role_arn → 400",
			jwtReq("GET", "/admin/bedrock/models", "acme", "owner", "", "", map[string]string{"role_arn": "arn:aws:iam::1:role/Nope"}),
			400, "AIPlatGatewayAccess",
		},
		{
			"unknown route → 404",
			jwtReq("GET", "/admin/nope", "acme", "owner", "", "", nil),
			404, "not found",
		},
	}
	for _, c := range cases {
		out, err := handle(ctx, c.req)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if out.StatusCode != c.wantStatus {
			t.Errorf("%s: status=%d, want %d (body=%s)", c.name, out.StatusCode, c.wantStatus, out.Body)
			continue
		}
		if c.wantErrSub != "" {
			var m map[string]interface{}
			json.Unmarshal([]byte(out.Body), &m)
			es, _ := m["error"].(string)
			if es == "" || !strings.Contains(es, c.wantErrSub) {
				t.Errorf("%s: error=%q does not contain %q", c.name, es, c.wantErrSub)
			}
		}
	}
}
