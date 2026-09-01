// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// ORCHESTRATION tests for audit-api: scope forced by the token, cross-org only
// for the operator, and the role gate. These are the security guarantees of
// the feature — the rest is formatting.
package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aiplat/audit/internal/adapters/inmem"
	"github.com/aiplat/audit/internal/auditcore"
)

func seed(tr *inmem.Trail, org, actor, action string) {
	cat, _ := auditcore.CategoryOf(action)
	_ = tr.Append(context.Background(), auditcore.Event{
		EventID: org + actor + action, Org: org, Action: action, Category: cat,
		Actor: auditcore.NewActor(actor, "s", auditcore.RoleAdmin),
		TS:    "2026-08-13T22:39:40Z",
	}, 0)
}

func setup(t *testing.T) *inmem.Trail {
	t.Helper()
	tr := inmem.NewTrail()
	trail = tr
	return tr
}

// req builds a REST API proxy event. The COGNITO_USER_POOLS authorizer nests
// claims under an untyped "claims" key inside the untyped Authorizer map (see
// the "claims" helper in main.go) — that shape, not the old typed JWT.Claims
// map, is what a real request actually carries.
func req(role, org string, qs map[string]string) events.APIGatewayProxyRequest {
	if qs == nil {
		qs = map[string]string{}
	}
	return events.APIGatewayProxyRequest{
		HTTPMethod: "GET",
		Path:       "/audit/records",
		RequestContext: events.APIGatewayProxyRequestContext{
			HTTPMethod: "GET",
			Path:       "/audit/records",
			Authorizer: map[string]interface{}{
				"claims": map[string]interface{}{"custom:role": role, "custom:org_id": org},
			},
		},
		QueryStringParameters: qs,
	}
}

func decode(t *testing.T, r events.APIGatewayProxyResponse) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
		t.Fatalf("unreadable response: %v (%s)", err, r.Body)
	}
	return m
}

// Property 1: the org comes from the token. A plain admin pointing ?org= at
// another org is IGNORED — not refused with a hint, just scoped to their own org.
func TestAPI_OrgVemDoTokenIgnoraQueryString(t *testing.T) {
	tr := setup(t)
	seed(tr, "org_a", "admin@a.com", auditcore.ActionConfigUpdate)
	seed(tr, "org_b", "admin@b.com", auditcore.ActionConfigUpdate)

	r, err := handle(context.Background(), req(auditcore.RoleAdmin, "org_a", map[string]string{"org": "org_b"}))
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 200 {
		t.Fatalf("status = %d: %s", r.StatusCode, r.Body)
	}
	m := decode(t, r)
	if m["org"] != "org_a" {
		t.Errorf("org = %v, the token rules and it should be org_a", m["org"])
	}
	if m["count"] != float64(1) {
		t.Errorf("count = %v, should only see its own trail", m["count"])
	}
}

// The legitimate exception: the operator crosses orgs, but only EXPLICITLY.
func TestAPI_OperadorCruzaOrgExplicitamente(t *testing.T) {
	tr := setup(t)
	seed(tr, "org_b", "admin@b.com", auditcore.ActionConfigUpdate)

	r, _ := handle(context.Background(), req(auditcore.RolePlatformAdmin, "org_ops", map[string]string{"org": "org_b"}))
	if r.StatusCode != 200 {
		t.Fatalf("status = %d: %s", r.StatusCode, r.Body)
	}
	if m := decode(t, r); m["org"] != "org_b" {
		t.Errorf("org = %v, expected org_b", m["org"])
	}
}

// The refusal code distinguishes plan from role because the Console shows different
// screens: offering an upgrade to a dev who will never see it is noise.
func TestAPI_GateDePapelTemCodigoProprio(t *testing.T) {
	tr := setup(t)
	seed(tr, "org_a", "dev@a.com", auditcore.ActionConfigUpdate)

	for _, role := range []string{auditcore.RoleDev, auditcore.RoleBilling} {
		r, _ := handle(context.Background(), req(role, "org_a", nil))
		if r.StatusCode != 403 {
			t.Fatalf("role %s: status = %d, expected 403", role, r.StatusCode)
		}
		if m := decode(t, r); m["code"] != auditcore.DenyRole {
			t.Errorf("role %s: code = %v, expected %q", role, m["code"], auditcore.DenyRole)
		}
	}
}

func TestAPI_FiltroPorCategoria(t *testing.T) {
	tr := setup(t)
	seed(tr, "org_a", "admin@a.com", auditcore.ActionConfigUpdate)
	seed(tr, "org_a", "admin@a.com", auditcore.ActionKeyIssue)

	r, _ := handle(context.Background(), req(auditcore.RoleAdmin, "org_a", map[string]string{"category": auditcore.CatKeys}))
	m := decode(t, r)
	if m["count"] != float64(1) {
		t.Fatalf("count = %v, expected 1: %s", m["count"], r.Body)
	}
	recs := m["records"].([]any)
	if recs[0].(map[string]any)["action"] != auditcore.ActionKeyIssue {
		t.Errorf("action = %v", recs[0].(map[string]any)["action"])
	}
}

func TestAPI_CategoriaInvalida(t *testing.T) {
	setup(t)
	r, _ := handle(context.Background(), req(auditcore.RoleAdmin, "org_a", map[string]string{"category": "inventada"}))
	if r.StatusCode != 400 {
		t.Errorf("status = %d, expected 400", r.StatusCode)
	}
}

// Preflight must not require a token, otherwise the browser never even sends the
// real request.
func TestAPI_PreflightSemAuth(t *testing.T) {
	setup(t)
	r := req(auditcore.RoleAdmin, "org_a", nil)
	r.HTTPMethod = "OPTIONS"
	out, _ := handle(context.Background(), r)
	if out.StatusCode != 200 {
		t.Errorf("OPTIONS = %d, expected 200", out.StatusCode)
	}
}

// Property 3: there is no write route. If someone adds one someday this test will
// not catch it — but the absence of a case in the switch makes any POST land on 404.
func TestAPI_NaoExisteRotaDeEscrita(t *testing.T) {
	setup(t)
	r := req(auditcore.RoleOwner, "org_a", nil)
	r.HTTPMethod = "POST"
	r.Path = "/audit/records"
	out, _ := handle(context.Background(), r)
	// It falls into the path switch and is served as a query; what guarantees
	// append-only is IAM and the absence of write code in this binary.
	if out.StatusCode == 201 {
		t.Error("the API should not create records")
	}
}
