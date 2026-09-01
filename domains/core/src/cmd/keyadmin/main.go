// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// keyadmin of the Core domain: issuing/managing API keys per org.
// The api-keys table is Core data (the router does the lookup at runtime); that is
// why management lives here and the console (Governance) consumes it through the
// admin contract.
//
// Security: we store only sha256(key). The plaintext is returned ONCE on creation.
//
// Routes (x-admin-token header):
//
//	POST   /admin/keys   {org, app_tag?}       → creates and returns {api_key} (once)
//	                      (`tenant` also accepted, legacy alias for `org`)
//	GET    /admin/keys                          → lists (without the secret)
//	DELETE /admin/keys   {id}                    → revokes (id = api_key_hash)
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
)

var (
	ddb   *dynamodb.Client
	table = os.Getenv("API_KEYS_TABLE")
	// There is no ADMIN_TOKEN here on purpose: the gate for this API is the API
	// Gateway COGNITO_USER_POOLS authorizer. Reading a token that is never
	// compared would mislead a reader into relaxing that authorizer.
	configTable = os.Getenv("CONFIG_TABLE") // gov-config table (contract): validates that team/app exist
)

// readOrgTree reads the org's teams/apps record (TEAMS#<org>) from the Governance
// config table — a CONTRACT READ by partition (the same pattern as the router
// reading config), never a synchronous call to the other domain's Lambda.
// ok=false when there is no record or the read fails (graceful degradation: keyadmin
// does NOT block issuing if it cannot validate).
func readOrgTree(ctx context.Context, org string) (teams, apps map[string]bool, ok bool) {
	if configTable == "" || org == "" {
		return nil, nil, false
	}
	out, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &configTable,
		Key:       map[string]ddbtypes.AttributeValue{"pk": s("TEAMS#" + org)},
	})
	if err != nil || out.Item == nil {
		return nil, nil, false
	}
	cv, _ := out.Item["config"].(*ddbtypes.AttributeValueMemberS)
	if cv == nil {
		return nil, nil, false
	}
	var tree struct {
		Teams map[string]json.RawMessage `json:"teams"`
		Apps  map[string]json.RawMessage `json:"apps"`
	}
	if json.Unmarshal([]byte(cv.Value), &tree) != nil {
		return nil, nil, false
	}
	teams = map[string]bool{}
	for k := range tree.Teams {
		teams[k] = true
	}
	apps = map[string]bool{}
	for k := range tree.Apps {
		apps[k] = true
	}
	return teams, apps, true
}

// allowedOrigins is the allowlist of browser origins permitted to READ this admin
// API's responses. Built once at cold start from CONSOLE_ORIGIN (comma separated —
// the Contract of Environment published by the frontend domain via SSM), the same
// pattern governance/config-api uses.
//
// Deny by default, never a wildcard: an Origin that is not on the list gets NO
// access-control-allow-origin header, so the browser blocks the response itself.
// That matters most here — this endpoint issues and revokes API keys, and a
// wildcard would let any page on the internet read those answers (and can never be
// combined with credentials anyway). Server-to-server callers are unaffected: CORS
// is enforced by browsers only.
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
// lowercased, but the lookup is case-insensitive because header names are
// case-insensitive on the wire and missing it would CORS-block a legitimate console.
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

// corsHeaders is a function of the REQUEST origin, not a package-level map: it
// echoes the caller's own origin only when it is on the allowlist, and adds
// `vary: Origin` so shared caches do not serve one origin's headers to another.
func corsHeaders(reqOrigin string) map[string]string {
	h := map[string]string{
		"access-control-allow-methods": "GET,POST,DELETE,OPTIONS",
		"access-control-allow-headers": "content-type,authorization,x-admin-token",
		"content-type":                 "application/json",
	}
	if reqOrigin != "" && allowedOrigins[reqOrigin] {
		h["access-control-allow-origin"] = reqOrigin
		h["vary"] = "Origin"
	}
	return h
}

func resp(reqOrigin string, status int, obj interface{}) (events.APIGatewayProxyResponse, error) {
	b, _ := json.Marshal(obj)
	return events.APIGatewayProxyResponse{StatusCode: status, Headers: corsHeaders(reqOrigin), Body: string(b)}, nil
}

// claim reads one Cognito claim out of the REST API's COGNITO_USER_POOLS authorizer
// context. Claims come nested under an untyped "claims" key inside the untyped
// Authorizer map (confirmed against a real request, not assumed from docs) —
// different from the HTTP API's JWT authorizer, which nested them under a typed
// Authorizer.JWT.Claims map. A missing authorizer, "claims" key, or individual
// claim all return "", the same safe default as before.
func claim(req events.APIGatewayProxyRequest, name string) string {
	if req.RequestContext.Authorizer == nil {
		return ""
	}
	claims, ok := req.RequestContext.Authorizer["claims"].(map[string]interface{})
	if !ok {
		return ""
	}
	if v, ok := claims[name]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func s(v string) *ddbtypes.AttributeValueMemberS { return &ddbtypes.AttributeValueMemberS{Value: v} }
func awsStr(v string) *string                    { return &v }

func genKey() string {
	buf := make([]byte, 24)
	rand.Read(buf)
	return "sk-aiplat-" + hex.EncodeToString(buf)
}

func handle(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	method := req.HTTPMethod
	// Captured once and threaded through every response: CORS is decided by the
	// allowlist above (deny by default, no wildcard), so every exit path needs the
	// request's own origin to be able to echo it.
	reqOrigin := originOf(req.Headers)
	if method == "OPTIONS" {
		return events.APIGatewayProxyResponse{StatusCode: 204, Headers: corsHeaders(reqOrigin)}, nil
	}
	// Isolation: the org comes from the Cognito CLAIMS (validated by API Gateway).
	// Only platform_admin can operate on another org, and only explicitly.
	claimOrg := claim(req, "custom:org_id")
	role := claim(req, "custom:role")
	claimTeam := claim(req, "team")
	isPlatform := role == "platform_admin"

	// Audit context: actor and origin come from the REQUEST (claims + request
	// context), never from the body. Authorship derived from input is forgeable.
	claimEmail := claim(req, "email")
	claimSub := claim(req, "sub")
	aud := auditCtx{
		actor:     newAuditActor(claimEmail, claimSub, role),
		sourceIP:  req.RequestContext.Identity.SourceIP,
		userAgent: req.RequestContext.Identity.UserAgent,
	}
	// Team enforcement (Slice B): a dev/billing user bound to a team only sees/creates/
	// revokes keys of their own team. Owner/admin/platform_admin operate on the whole org.
	teamScoped := claimTeam != "" && role != "owner" && role != "admin" && !isPlatform
	// App scope (Pro, per-user access): a user with apps in the token only
	// sees/creates/revokes keys of their own apps.
	appsClaim := claim(req, "apps")
	appScoped := appsClaim != "" && role != "owner" && role != "admin" && !isPlatform
	appSet := map[string]bool{}
	for _, a := range strings.Split(appsClaim, ",") {
		if a = strings.TrimSpace(a); a != "" {
			appSet[a] = true
		}
	}
	// Role: billing does not issue/revoke keys; dev can (within their scope).
	canKeys := isPlatform || role == "owner" || role == "admin" || role == "dev"

	// scopeOrg resolves the operation's effective org.
	scopeOrg := func(param string) (string, bool) {
		if isPlatform {
			if param == "" {
				return "", false
			}
			return param, true
		}
		if claimOrg == "" {
			return "", false
		}
		return claimOrg, true // org user: the parameter is ignored
	}

	switch method {
	case "POST":
		if !canKeys {
			return resp(reqOrigin, 403, map[string]string{"error": "your role cannot issue keys (billing is read-only)"})
		}
		var b struct {
			Org    string `json:"org"`
			Tenant string `json:"tenant"` // compat: alias for org
			Team   string `json:"team"`
			App    string `json:"app"`
			AppTag string `json:"app_tag"` // compat: alias for app
		}
		if json.Unmarshal([]byte(req.Body), &b) != nil {
			return resp(reqOrigin, 400, map[string]string{"error": "invalid JSON"})
		}
		param := b.Org
		if param == "" {
			param = b.Tenant
		}
		org, ok := scopeOrg(param)
		if !ok {
			return resp(reqOrigin, 400, map[string]string{"error": "org could not be determined (platform_admin must provide org)"})
		}
		team := b.Team
		if team == "" {
			team = "default"
		}
		// A team-scoped user does not issue keys for another team: force the claim's team.
		if teamScoped {
			team = claimTeam
		}
		app := b.App
		// App-scoped (Pro): the key must belong to an app the user is allowed to use.
		if appScoped && app != "" && !appSet[app] {
			return resp(reqOrigin, 403, map[string]string{"error": "you do not have access to this app"})
		}
		if appScoped && app == "" && len(appSet) == 1 {
			for a := range appSet {
				app = a
			} // sensible default: the user's only app
		}
		if app == "" {
			app = b.AppTag
		}
		// Existence validation (defense in depth). If the org already keeps a
		// teams/apps record, the key may only point at items that EXIST — the primary
		// enforcement is the console selector, this is the backstop.
		// Compat/degradation: an empty or unreadable record => accept (with 'default'
		// always valid), so orgs that have not used the management UI yet do not break.
		if teams, apps, okTree := readOrgTree(ctx, org); okTree && len(teams) > 0 {
			if team != "default" && !teams[team] {
				return resp(reqOrigin, 400, map[string]string{"error": "team does not exist — create the team under Teams & Apps before issuing the key"})
			}
			if app != "" && app != "default" && len(apps) > 0 && !apps[app] {
				return resp(reqOrigin, 400, map[string]string{"error": "app does not exist — create the app under Teams & Apps before issuing the key"})
			}
		}
		key := genKey()
		sum := sha256.Sum256([]byte(key))
		hash := hex.EncodeToString(sum[:])
		created := time.Now().UTC().Format(time.RFC3339)
		prefix := key[:14] + "…"
		_, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: &table, Item: map[string]ddbtypes.AttributeValue{
			"api_key_hash": s(hash),
			"org_id":       s(org), "team_id": s(team), "app": s(app),
			"tenant": s(org), "app_tag": s(app), // compat with old readers
			"status": s("active"), "created_at": s(created), "key_prefix": s(prefix),
		}})
		if err != nil {
			return resp(reqOrigin, 500, map[string]string{"error": err.Error()})
		}
		emitAudit(ctx, aud, audKeyIssue, org, team, app, prefix)
		// api_key is returned only here, once.
		return resp(reqOrigin, 200, map[string]string{"api_key": key, "key_prefix": prefix,
			"org": org, "team": team, "app": app, "created_at": created})

	case "GET":
		// Isolation: an org scope is required. Without an org, return nothing (avoids a
		// cross-org dump).
		param := req.QueryStringParameters["org"]
		if param == "" {
			param = req.QueryStringParameters["tenant"]
		}
		org, ok := scopeOrg(param)
		if !ok {
			return resp(reqOrigin, 400, map[string]string{"error": "org could not be determined (platform_admin must provide ?org=)"})
		}
		out, err := ddb.Scan(ctx, &dynamodb.ScanInput{
			TableName:                 &table,
			FilterExpression:          awsStr("org_id = :o OR tenant = :o"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":o": s(org)},
		})
		if err != nil {
			return resp(reqOrigin, 500, map[string]string{"error": err.Error()})
		}
		// ?summary=1: the console's Teams & Apps / Members tabs only need the
		// DISTINCT team/app names to populate a <select> — not every key. An org
		// with hundreds of keys (real usage, or a stress-test leftover) made the
		// full list slow enough client-side to look hung. Same auth/scoping as
		// the full list below, just a smaller response shape.
		summary := req.QueryStringParameters["summary"] == "1"
		teamSet, appSet2 := map[string]bool{}, map[string]bool{}
		keys := []map[string]string{}
		for _, it := range out.Items {
			get := func(k string) string {
				if v, ok := it[k].(*ddbtypes.AttributeValueMemberS); ok {
					return v.Value
				}
				return ""
			}
			team := get("team_id")
			if team == "" {
				team = "default"
			}
			// A team-scoped user only sees their own team's keys.
			if teamScoped && team != claimTeam {
				continue
			}
			app := get("app")
			if app == "" {
				app = get("app_tag")
			}
			// An app-scoped user (Pro) only sees the keys of their own apps.
			if appScoped && !appSet[app] {
				continue
			}
			if summary {
				teamSet[team] = true
				if app != "" {
					appSet2[app] = true
				}
				continue
			}
			status := get("status")
			if status == "" {
				status = "active"
			}
			keys = append(keys, map[string]string{
				"id": get("api_key_hash"), "org": get("org_id"), "team": team, "app": app,
				"status": status, "key_prefix": get("key_prefix"), "created_at": get("created_at"),
			})
		}
		if summary {
			teams := make([]string, 0, len(teamSet))
			for t := range teamSet {
				teams = append(teams, t)
			}
			apps := make([]string, 0, len(appSet2))
			for a := range appSet2 {
				apps = append(apps, a)
			}
			sort.Strings(teams)
			sort.Strings(apps)
			return resp(reqOrigin, 200, map[string]interface{}{"org": org, "teams": teams, "apps": apps, "count": len(out.Items)})
		}
		return resp(reqOrigin, 200, map[string]interface{}{"org": org, "keys": keys})

	case "DELETE":
		if !canKeys {
			return resp(reqOrigin, 403, map[string]string{"error": "your role cannot revoke keys (billing is read-only)"})
		}
		var b struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(req.Body), &b) != nil || b.ID == "" {
			return resp(reqOrigin, 400, map[string]string{"error": "id is required"})
		}
		// OWNERSHIP check: without it, an org admin could revoke another org's key
		// just by knowing the hash.
		cur, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{TableName: &table,
			Key: map[string]ddbtypes.AttributeValue{"api_key_hash": s(b.ID)}})
		if err != nil {
			return resp(reqOrigin, 500, map[string]string{"error": err.Error()})
		}
		if cur.Item == nil {
			return resp(reqOrigin, 404, map[string]string{"error": "key not found"})
		}
		owner := ""
		if v, ok := cur.Item["org_id"].(*ddbtypes.AttributeValueMemberS); ok {
			owner = v.Value
		}
		if owner == "" {
			if v, ok := cur.Item["tenant"].(*ddbtypes.AttributeValueMemberS); ok {
				owner = v.Value
			}
		}
		if !isPlatform && owner != claimOrg {
			return resp(reqOrigin, 403, map[string]string{"error": "key belongs to another org"})
		}
		// Team-scoped: ownership also verified at the team level.
		if teamScoped {
			kt := "default"
			if v, ok := cur.Item["team_id"].(*ddbtypes.AttributeValueMemberS); ok && v.Value != "" {
				kt = v.Value
			}
			if kt != claimTeam {
				return resp(reqOrigin, 403, map[string]string{"error": "key belongs to another team"})
			}
		}
		// App-scoped (Pro): ownership also verified at the app level.
		if appScoped {
			ka := ""
			if v, ok := cur.Item["app"].(*ddbtypes.AttributeValueMemberS); ok {
				ka = v.Value
			}
			if ka == "" {
				if v, ok := cur.Item["app_tag"].(*ddbtypes.AttributeValueMemberS); ok {
					ka = v.Value
				}
			}
			if !appSet[ka] {
				return resp(reqOrigin, 403, map[string]string{"error": "key belongs to another app"})
			}
		}
		// Key identification for the audit trail, read BEFORE the delete: afterwards the
		// item no longer exists. Only prefix, team and app — never the key.
		audPrefix, audTeam, audApp := "", "", ""
		if v, ok := cur.Item["key_prefix"].(*ddbtypes.AttributeValueMemberS); ok {
			audPrefix = v.Value
		}
		if v, ok := cur.Item["team_id"].(*ddbtypes.AttributeValueMemberS); ok {
			audTeam = v.Value
		}
		if v, ok := cur.Item["app"].(*ddbtypes.AttributeValueMemberS); ok {
			audApp = v.Value
		}

		if _, err := ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &table,
			Key: map[string]ddbtypes.AttributeValue{"api_key_hash": s(b.ID)}}); err != nil {
			return resp(reqOrigin, 500, map[string]string{"error": err.Error()})
		}
		// Emitted AFTER the revocation succeeded: auditing something that did not happen
		// would be worse than not auditing. It never fails the response.
		emitAudit(ctx, aud, audKeyRevoke, owner, audTeam, audApp, audPrefix)
		return resp(reqOrigin, 200, map[string]string{"status": "revoked", "org": owner})
	}
	return resp(reqOrigin, 404, map[string]string{"error": "not found"})
}

func main() {
	cfg, _ := awscfg.LoadDefaultConfig(context.TODO())
	ddb = dynamodb.NewFromConfig(cfg)
	eb = eventbridge.NewFromConfig(cfg)
	lambda.Start(handle)
}
