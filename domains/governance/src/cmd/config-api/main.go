// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// config-api of the Governance domain, in Go (arm64/Graviton).
// Source of truth for the dynamic config (auto-cheapest, routing, pricing, cache)
// and for provider credentials (Secrets Manager, prefix aiplat/gateway/*).
//
// REFACTORED FOR SINGLE-ORG DEPLOYMENT:
// - Removed dynamic org provisioning from member creation
// - Org must be initialized at deployment time via bootstrap
// - No self-service signup endpoints (removed from this API)
// - Simplified member invitation to always use token-based org scope
//
// Routes (protected by a Cognito user-pool authorizer; the JWT carries
// custom:org_id / custom:role / team, which drive scope and role checks):
//
//	GET  /admin/config    → current config (JSON)
//	PUT  /admin/config    → writes the new config
//	POST /admin/secrets    → creates/updates a provider credential. Body: {name, api_key}
//
// The legacy x-admin-token header was removed in 1.5.0 when the API moved to
// Cognito; it survives only in CHANGELOG history.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aiplat/governance/internal/adapters/cognitosigv4"
	"github.com/aiplat/governance/internal/adapters/smsecrets"
	"github.com/aiplat/governance/internal/govcore"
	"github.com/aiplat/governance/internal/ports"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

var (
	ddb       *dynamodb.Client
	sm        *secretsmanager.Client
	stsClient *sts.Client
	baseCfg   aws.Config
	// CheckRedirect refuses to follow redirects (GOV-SEC-006): a provider-model
	// listing carries the org's credential (x-api-key / Bearer), and Go strips only
	// Authorization on a cross-host redirect — an x-api-key would follow a 302 to an
	// attacker host. Not following at all closes that, and AWS JSON APIs never redirect.
	httpc = &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// identity and secrets are the outbound PORTS (identity control plane and
	// credential writing). The shell depends only on these interfaces; the
	// concrete adapters (cognitosigv4/smsecrets) are wired in main().
	identity    ports.Identity
	secrets     ports.SecretStore
	rateLimiter *RateLimiter
	configTable = os.Getenv("CONFIG_TABLE")
	// There is no ADMIN_TOKEN here on purpose: the gate for this API is the API
	// Gateway COGNITO_USER_POOLS authorizer. Reading a token that is never
	// compared would mislead a reader into relaxing that authorizer.
	userPool       = os.Getenv("USER_POOL_ID")
	secretPrefix   = envOr("SECRET_PREFIX", "aiplat/gateway/")
	rateLimitTable = envOr("RATE_LIMIT_TABLE", "")
	// limitsTable is the Core's counter table. We read it by PARTITION
	// (CREDIT#<org>#<provider>) to show the estimated credit consumption —
	// a per-org contract read, the same pattern as billing reading the Cost_Store.
	// There is never a synchronous call to the other domain's Lambda.
	limitsTable = os.Getenv("LIMITS_TABLE")
	// auditTable holds the control plane audit trail (Governance's own store).
	// pk=org, sk sortable by time. Never stores a password/token.
	auditTable = os.Getenv("AUDIT_TABLE")
)

// roles a customer may assign to a member (never platform_admin).
var memberRoles = map[string]bool{"owner": true, "admin": true, "billing": true, "dev": true}

// --- Authorization Context (IDOR Prevention) ---

// AuthContext holds the authenticated user's context
type AuthContext struct {
	Org        string
	Email      string
	Role       string
	Team       string
	Access     govcore.Access
	IsPlatform bool
}

// NewAuthContext creates an auth context from JWT claims
func NewAuthContext(claims map[string]string) AuthContext {
	org := claims["custom:org_id"]
	role := claims["custom:role"]
	team := claims["team"]
	email := claims["email"]

	c := govcore.Claims{Org: org, Role: role, Team: team, Email: email}
	access := govcore.Resolve(c)

	return AuthContext{
		Org:        org,
		Email:      email,
		Role:       role,
		Team:       team,
		Access:     access,
		IsPlatform: access.IsPlatform,
	}
}

// CanAccessOrg checks if the user can access the specified org
func (ac AuthContext) CanAccessOrg(targetOrg string) bool {
	// Platform admin can access any org
	if ac.IsPlatform {
		return true
	}

	// Regular users can only access their own org
	return ac.Org == targetOrg
}

// CanAccessTeam checks if the user can access the specified team
func (ac AuthContext) CanAccessTeam(targetOrg, targetTeam string) bool {
	// Must be in the same org
	if !ac.CanAccessOrg(targetOrg) {
		return false
	}

	// Team-scoped users can only access their own team
	if ac.Access.TeamScoped {
		return ac.Team == targetTeam
	}

	// Admins and owners can access all teams in their org
	return ac.Access.CanAdmin
}

// CanModifyMember checks if the user can modify another member
func (ac AuthContext) CanModifyMember(targetOrg, targetEmail string) bool {
	// Must be in the same org
	if !ac.CanAccessOrg(targetOrg) {
		return false
	}

	// Must have admin privileges
	if !ac.Access.CanAdmin {
		return false
	}

	// Team-scoped admins can only modify members in their team
	if ac.Access.TeamScoped {
		// Would need to look up target member's team
		// For simplicity, team-scoped users cannot modify members
		return false
	}

	return true
}

// MaxRequestBodySize limits the size of request bodies (1MB)
const MaxRequestBodySize = 1 * 1024 * 1024

// RateLimiter implements token bucket rate limiting using DynamoDB
type RateLimiter struct {
	table string
	ddb   *dynamodb.Client
}

// CheckRateLimit verifies if the request is within rate limits
func (rl *RateLimiter) CheckRateLimit(ctx context.Context, identifier string, limit int64, window time.Duration) (bool, error) {
	if rl == nil || rl.table == "" || rl.ddb == nil {
		// Rate limiting not configured (e.g. in unit tests where main() never
		// ran), allow request
		return true, nil
	}

	// Create a unique key for this identifier and time window
	now := time.Now()
	windowKey := now.Unix() / int64(window.Seconds())
	key := fmt.Sprintf("ratelimit:%s:%d", identifier, windowKey)
	ttl := now.Add(window * 2).Unix() // TTL longer than window to handle edge cases

	// Increment counter in DynamoDB
	out, err := rl.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &rl.table,
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: key},
		},
		UpdateExpression: aws.String("ADD #count :inc SET #ttl = if_not_exists(#ttl, :ttl)"),
		ExpressionAttributeNames: map[string]string{
			"#count": "count",
			"#ttl":   "ttl",
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":inc": &ddbtypes.AttributeValueMemberN{Value: "1"},
			":ttl": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(ttl, 10)},
		},
		ReturnValues: ddbtypes.ReturnValueAllNew,
	})
	if err != nil {
		return false, err
	}

	// Check current count
	if countAttr, ok := out.Attributes["count"].(*ddbtypes.AttributeValueMemberN); ok {
		count, _ := strconv.ParseInt(countAttr.Value, 10, 64)
		return count <= limit, nil
	}

	return true, nil
}

// hashIdentifier creates a hashed version of the identifier for privacy
func hashIdentifier(identifier string) string {
	hash := sha256.Sum256([]byte(identifier))
	return hex.EncodeToString(hash[:])
}

// ValidationError represents a validation failure
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ReadAndValidateJSON reads and validates JSON from request body
func ReadAndValidateJSON(body string, v interface{}) error {
	// Check size
	if len(body) > MaxRequestBodySize {
		return fmt.Errorf("request body too large: %d bytes (max %d)",
			len(body), MaxRequestBodySize)
	}

	// Check for null bytes (possible injection)
	if strings.Contains(body, "\x00") {
		return fmt.Errorf("request body contains null bytes")
	}

	// Unmarshal with strict validation
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields() // Reject unknown fields

	if err := decoder.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	return nil
}

// Helper validation functions
func isValidEmail(email string) bool {
	return len(email) > 3 &&
		strings.Contains(email, "@") &&
		strings.Contains(email, ".") &&
		!strings.Contains(email, " ")
}

func isValidIdentifier(s string) bool {
	// Allow only alphanumeric, dash, underscore
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_') {
			return false
		}
	}
	return len(s) > 0
}

// maskEmail redacts the local part of an address for non-admin rosters
// (GOV-SEC-008): "alice@example.com" -> "a***@example.com". A value without a
// single "@" is replaced wholesale so nothing identifiable leaks.
func maskEmail(e string) string {
	at := strings.IndexByte(e, '@')
	if at <= 0 {
		return "***"
	}
	local := e[:at]
	first := local[:1]
	return first + "***" + e[at:]
}

// TeamMetaValidator validates TeamMeta input
func validateTeamMeta(tm struct {
	DisplayName string `json:"display_name"`
}) []ValidationError {
	var errors []ValidationError

	// Validate name
	if tm.DisplayName == "" {
		errors = append(errors, ValidationError{
			Field:   "display_name",
			Message: "display_name is required",
		})
	}
	if len(tm.DisplayName) > 100 {
		errors = append(errors, ValidationError{
			Field:   "display_name",
			Message: "display_name must be 100 characters or less",
		})
	}
	if !utf8.ValidString(tm.DisplayName) {
		errors = append(errors, ValidationError{
			Field:   "display_name",
			Message: "display_name contains invalid UTF-8",
		})
	}
	// GOV-SEC-009: the SQL/script-style blocklist was removed. DynamoDB access uses
	// parameterised expressions (no string-built queries), so the blocklist bought no
	// safety and it rejected legitimate display names like "O'Brien" or "A/B test".
	// UTF-8 and length validation above are the real guards.

	return errors
}

// MemberUpdateValidator validates member update requests
func validateMemberUpdate(updates struct {
	Email    string   `json:"email"`
	Password string   `json:"password"`
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Team     string   `json:"team"`
	Apps     []string `json:"apps"`
}) []ValidationError {
	var errs []ValidationError

	// Validate email
	if updates.Email != "" && !isValidEmail(updates.Email) {
		errs = append(errs, ValidationError{
			Field:   "email",
			Message: "invalid email format",
		})
	}

	// Validate role
	if updates.Role != "" && !memberRoles[updates.Role] {
		errs = append(errs, ValidationError{
			Field:   "role",
			Message: fmt.Sprintf("invalid role: %s", updates.Role),
		})
	}

	// Validate team. GOV-SEC-009: a team id goes straight into the scope key
	// (ORG#<org>#TEAM#<team>), so it must be charset-checked — a `#` would alias a
	// different scope. Length alone is not enough.
	if len(updates.Team) > 50 {
		errs = append(errs, ValidationError{
			Field:   "team",
			Message: "team name too long",
		})
	}
	if updates.Team != "" && !isValidIdentifier(updates.Team) {
		errs = append(errs, ValidationError{
			Field:   "team",
			Message: "invalid team identifier (letters, digits, '-' and '_' only)",
		})
	}

	// Validate name
	if len(updates.Name) > 200 {
		errs = append(errs, ValidationError{
			Field:   "name",
			Message: "name too long (max 200 characters)",
		})
	}

	// Validate apps array
	if len(updates.Apps) > 100 {
		errs = append(errs, ValidationError{
			Field:   "apps",
			Message: "too many apps (max 100)",
		})
	}
	for i, app := range updates.Apps {
		if len(app) > 50 {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("apps[%d]", i),
				Message: "app name too long",
			})
		}
		if !isValidIdentifier(app) {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("apps[%d]", i),
				Message: "invalid app identifier",
			})
		}
	}

	return errs
}

// validateCreditsUpdate validates credit update requests
func validateCreditsUpdate(updates struct {
	Provider  string   `json:"provider"`
	AmountUSD *float64 `json:"amount_usd"`
	ExpiresAt *string  `json:"expires_at"`
	Corrected *float64 `json:"corrected_remaining_usd"`
}) []ValidationError {
	var errs []ValidationError

	if updates.Provider == "" {
		errs = append(errs, ValidationError{
			Field:   "provider",
			Message: "provider is required",
		})
	}

	if len(updates.Provider) > 50 {
		errs = append(errs, ValidationError{
			Field:   "provider",
			Message: "provider name too long",
		})
	}

	if updates.AmountUSD != nil && !govcore.NonNegative(*updates.AmountUSD) {
		errs = append(errs, ValidationError{
			Field:   "amount_usd",
			Message: "amount_usd cannot be negative",
		})
	}

	if updates.Corrected != nil && !govcore.NonNegative(*updates.Corrected) {
		errs = append(errs, ValidationError{
			Field:   "corrected_remaining_usd",
			Message: "corrected_remaining_usd cannot be negative",
		})
	}

	if updates.ExpiresAt != nil && !govcore.ValidDate(*updates.ExpiresAt) {
		errs = append(errs, ValidationError{
			Field:   "expires_at",
			Message: "expires_at must be YYYY-MM-DD",
		})
	}

	return errs
}

// validateSecretRequest validates secret/credential requests
func validateSecretRequest(req struct {
	Name     string `json:"name"`
	APIKey   string `json:"api_key"`
	Value    string `json:"value"`
	Provider string `json:"provider"`
	Org      string `json:"org"`
}) []ValidationError {
	var errs []ValidationError

	key := req.APIKey
	if key == "" {
		key = req.Value
	}

	if key == "" {
		errs = append(errs, ValidationError{
			Field:   "api_key",
			Message: "api_key is required",
		})
	}

	if len(key) > 5000 {
		errs = append(errs, ValidationError{
			Field:   "api_key",
			Message: "api_key too long (max 5000 characters)",
		})
	}

	if req.Provider != "" {
		if len(req.Provider) > 50 {
			errs = append(errs, ValidationError{
				Field:   "provider",
				Message: "provider name too long",
			})
		}
	} else {
		if req.Name == "" {
			errs = append(errs, ValidationError{
				Field:   "name",
				Message: "name is required when provider is not specified",
			})
		}
		if len(req.Name) > 200 {
			errs = append(errs, ValidationError{
				Field:   "name",
				Message: "name too long (max 200 characters)",
			})
		}
	}

	return errs
}

// memberMetaKey: the member's access record (team + apps), kept separate from
// Cognito (which stores only org_id/role). GOV-SEC-003: the record is keyed by the
// member's immutable Cognito `sub`, NOT the e-mail — a member can change and re-verify
// their e-mail, and an e-mail-keyed scope let a team-scoped member escape their scope by
// moving to an address with no MEMBER# record. The sub never changes. pk
// MEMBER#<org>#<sub> does not collide with config scopes (ORG#/TEAM#/APP#) nor with the
// backoffice org scan. sub is a uuid, so no case normalisation is applied (unlike e-mail).
func memberMetaKey(org, sub string) string {
	return "MEMBER#" + org + "#" + sub
}

// readMemberMeta returns the member's {team, apps} (empty when absent), by sub.
func readMemberMeta(ctx context.Context, org, sub string) (string, []string) {
	m := readScope(ctx, memberMetaKey(org, sub))
	if m == nil {
		return "", nil
	}
	team, _ := m["team"].(string)
	var apps []string
	if raw, ok := m["apps"].([]interface{}); ok {
		for _, a := range raw {
			if s, ok := a.(string); ok {
				apps = append(apps, s)
			}
		}
	}
	return team, apps
}

// writeMemberMeta stores the member's team + apps (replacing merge), keyed by sub.
func writeMemberMeta(ctx context.Context, org, sub, team string, apps []string) error {
	body := map[string]interface{}{"team": team, "apps": apps, "updated_at": time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.Marshal(body)
	_, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: &configTable, Item: map[string]ddbtypes.AttributeValue{
		"pk":     &ddbtypes.AttributeValueMemberS{Value: memberMetaKey(org, sub)},
		"config": &ddbtypes.AttributeValueMemberS{Value: string(b)},
	}})
	return err
}

// memberTeamOK reports whether a member's requested team is assignable: either no
// team ("") or "default", or a team id that EXISTS in the org registry (GOV-SEC-009).
// The charset check in validateMemberUpdate stops a team string from aliasing a
// different scope key; this check stops a member from being parked on a team that
// was never created — which would silently scope their token to an empty policy.
func memberTeamOK(ctx context.Context, org, team string) bool {
	if team == "" || team == "default" {
		return true
	}
	tree := readOrgTree(ctx, org)
	_, ok := tree.Teams[team]
	return ok
}

// subFor resolves a member's immutable Cognito sub from their e-mail, for the MEMBER#
// record key (GOV-SEC-003). Returns ("", false) when the user cannot be read.
func subFor(ctx context.Context, email string) (string, bool) {
	sub, ok, err := identity.GetUserSub(ctx, email)
	if err != nil || !ok || sub == "" {
		return "", false
	}
	return sub, true
}

// --- Team/app registry (team-app-management feature) ---
//
// Team/app metadata lives in a SINGLE item per org: pk TEAMS#<org> (the partition
// carries the org → structural isolation). The team's POLICY (budget/allowed_models)
// stays in the ORG#…TEAM#<id> config scope, written by PUT /admin/config.
// The TEAMS# prefix does not collide with config scopes nor with MEMBER#.
func orgTreeKey(org string) string { return "TEAMS#" + org }

// readOrgTree reads the org's registry (zero value when absent — a new org).
func readOrgTree(ctx context.Context, org string) govcore.OrgTree {
	t, _ := readOrgTreeVersioned(ctx, org)
	return t
}

// readOrgTreeVersioned reads the org's registry along with its version (0 when
// the item does not exist yet — a brand new org). The version is what
// updateOrgTree uses to detect a concurrent write (see its doc comment).
func readOrgTreeVersioned(ctx context.Context, org string) (govcore.OrgTree, int64) {
	var t govcore.OrgTree
	out, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &configTable,
		Key:       map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: orgTreeKey(org)}},
	})
	var version int64
	if err == nil && out.Item != nil {
		if v, ok := out.Item["config"].(*ddbtypes.AttributeValueMemberS); ok {
			_ = json.Unmarshal([]byte(v.Value), &t)
		}
		if v, ok := out.Item["version"].(*ddbtypes.AttributeValueMemberN); ok {
			version, _ = strconv.ParseInt(v.Value, 10, 64)
		}
	}
	if t.Teams == nil {
		t.Teams = map[string]govcore.TeamMeta{}
	}
	if t.Apps == nil {
		t.Apps = map[string]govcore.AppMeta{}
	}
	return t, version
}

// writeOrgTree persists the org's entire registry UNCONDITIONALLY (last
// writer wins). Kept only for callers outside the read-modify-write pattern;
// every team/app mutation route must go through updateOrgTree instead — see
// its doc comment for why an unconditional PutItem here is a correctness bug.
func writeOrgTree(ctx context.Context, org string, t govcore.OrgTree) error {
	b, _ := json.Marshal(t)
	_, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: &configTable, Item: map[string]ddbtypes.AttributeValue{
		"pk":     &ddbtypes.AttributeValueMemberS{Value: orgTreeKey(org)},
		"config": &ddbtypes.AttributeValueMemberS{Value: string(b)},
	}})
	return err
}

// maxOrgTreeRetries bounds the optimistic-concurrency retry loop in
// updateOrgTree. Each attempt is a full GetItem+PutItem round trip; a real
// collision resolves within one or two retries, so this is a generous ceiling
// against a genuinely stuck client, not a tuning knob for normal operation.
const maxOrgTreeRetries = 8

// updateOrgTree performs a safe read-modify-write of the org's team/app
// registry under optimistic concurrency.
//
// KNOWN BUG this fixes: the registry is a SINGLE DynamoDB item per org
// (pk=TEAMS#<org>, see orgTreeKey). Before this fix, every mutation route
// (team/app create, rename, archive, remove) did a plain
// readOrgTree -> pure transform -> writeOrgTree(PutItem, unconditional). Two
// concurrent requests — even for DIFFERENT teams — could race: the second
// request's PutItem overwrites whatever the first one just added, because it
// read the tree BEFORE the first write landed. In practice this made
// "POST /admin/apps succeeds (200), but the key issued for that app minutes
// later fails with 'app does not exist'" a reproducible failure under any
// concurrent admin activity, not just a test artifact — confirmed with the
// load-test harness (scripts/loadtest/, 8-way concurrent app creation
// reproduced it within the first two teams every run).
//
// Fix: a numeric `version` attribute lives alongside the serialized tree in
// the SAME item. Every write is conditioned on the version it read still
// being current (`ConditionExpression: "version = :v"`, or
// `attribute_not_exists(version)` for a brand-new org with no item yet). If
// the condition fails — someone else wrote in between — this function
// retries the ENTIRE read-modify-write (re-reading the now-current tree and
// re-running `fn` against it), up to maxOrgTreeRetries times. `fn` must be
// side-effect-free beyond the tree it returns, since it can run more than
// once per call.
func updateOrgTree(ctx context.Context, org string, fn func(govcore.OrgTree) (govcore.OrgTree, error)) (govcore.OrgTree, error) {
	var lastErr error
	for attempt := 0; attempt < maxOrgTreeRetries; attempt++ {
		t, version := readOrgTreeVersioned(ctx, org)
		nt, err := fn(t)
		if err != nil {
			return govcore.OrgTree{}, err // business error (ErrExists/ErrNotFound/...): never retried
		}
		b, _ := json.Marshal(nt)
		input := &dynamodb.PutItemInput{
			TableName: &configTable,
			Item: map[string]ddbtypes.AttributeValue{
				"pk":      &ddbtypes.AttributeValueMemberS{Value: orgTreeKey(org)},
				"config":  &ddbtypes.AttributeValueMemberS{Value: string(b)},
				"version": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(version+1, 10)},
			},
		}
		if version == 0 {
			input.ConditionExpression = aws.String("attribute_not_exists(version)")
		} else {
			input.ConditionExpression = aws.String("version = :v")
			input.ExpressionAttributeValues = map[string]ddbtypes.AttributeValue{
				":v": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(version, 10)},
			}
		}
		_, err = ddb.PutItem(ctx, input)
		if err == nil {
			return nt, nil
		}
		var ccf *ddbtypes.ConditionalCheckFailedException
		if !errors.As(err, &ccf) {
			return govcore.OrgTree{}, err // real DynamoDB error (throttling, etc.): surface it
		}
		lastErr = err // conditional check failed: someone else wrote first — retry
		// Small jittered backoff: without it, every loser of a race retries in
		// lockstep and can collide again immediately under sustained high
		// concurrency on the SAME team/org. The jitter spreads retries out so
		// the retry itself doesn't manufacture the next collision.
		time.Sleep(time.Duration(10+rand.Intn(40)) * time.Millisecond)
	}
	return govcore.OrgTree{}, fmt.Errorf("updateOrgTree: exhausted %d retries under contention: %w", maxOrgTreeRetries, lastErr)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// allowedOrigins is the allowlist of origins permitted to access the API.
// Built at cold start (not a literal map) from CONSOLE_ORIGIN — the deployment's
// own console URL (Contract of Environment, published by the frontend domain via
// SSM; see governance/tf's local.console_url). A literal example.com placeholder
// here would silently CORS-block every real console, exactly what happened before
// this env var existed: the browser's fetch() fails with "NetworkError" on every
// PUT/POST (Save buttons) because the Lambda answers with an origin the browser
// never asked for, and the browser drops the response before JS ever sees it.
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

// getSecurityHeaders returns standard security headers for all responses
func getSecurityHeaders() map[string]string {
	return map[string]string{
		// Prevent MIME sniffing
		"X-Content-Type-Options": "nosniff",

		// Prevent clickjacking
		"X-Frame-Options": "DENY",

		// XSS Protection (legacy browsers)
		"X-XSS-Protection": "1; mode=block",

		// Content Security Policy
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",

		// HSTS - Force HTTPS for 1 year, include subdomains
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains; preload",

		// Referrer Policy - Don't leak URLs
		"Referrer-Policy": "no-referrer",

		// Permissions Policy (formerly Feature-Policy)
		"Permissions-Policy": "geolocation=(), microphone=(), camera=()",

		// Cache Control for sensitive data
		"Cache-Control": "no-store, no-cache, must-revalidate, private",
		"Pragma":        "no-cache",
		"Expires":       "0",
	}
}

// mergeHeaders merges multiple header maps (later maps override earlier ones)
func mergeHeaders(maps ...map[string]string) map[string]string {
	result := make(map[string]string)
	for _, m := range maps {
		for k, v := range m {
			result[k] = v
		}
	}
	return result
}

// getCORSHeaders returns CORS headers based on the request origin
func getCORSHeaders(origin string) map[string]string {
	headers := map[string]string{
		"access-control-allow-methods": "GET,PUT,POST,DELETE,OPTIONS",
		"access-control-allow-headers": "content-type,authorization",
		"content-type":                 "application/json",
		"access-control-max-age":       "86400", // Cache preflight for 24 hours
	}

	// Only set allowed origin if it's in the allowlist. Deny by default: omitting
	// the header (rather than echoing a placeholder origin) is what makes the
	// browser itself block the response — the correct behavior for a caller that
	// was never granted access, and no fake domain to keep in sync.
	if allowedOrigins[origin] {
		headers["access-control-allow-origin"] = origin
		// When credentials are used, you cannot use wildcard
		headers["access-control-allow-credentials"] = "true"
	}

	// Merge with security headers
	return mergeHeaders(headers, getSecurityHeaders())
}

type member struct {
	Email   string   `json:"email"`
	Role    string   `json:"role"`
	Status  string   `json:"status"`
	Enabled bool     `json:"enabled"`
	Team    string   `json:"team"`
	Apps    []string `json:"apps"`
}

// listMembers returns the org's members: Cognito users (via the Identity port)
// merged with the access record (team + apps) stored under MEMBER# in the config.
// The merge is shell orchestration — the identity port does not carry team/apps.
func listMembers(ctx context.Context, org string) ([]member, error) {
	users, err := identity.ListUsers(ctx, org)
	if err != nil {
		return nil, err
	}
	var out []member
	for _, u := range users {
		// GOV-SEC-003: the access record is keyed by the immutable sub. ListUsers
		// already carries it, so no extra AdminGetUser round-trip is needed here.
		team, apps := readMemberMeta(ctx, org, u.Sub)
		out = append(out, member{Email: u.Email, Role: u.Role, Status: u.Status, Enabled: u.Enabled, Team: team, Apps: apps})
	}
	return out, nil
}

// writeAudit stores an audit record (best-effort). An audit failure does NOT
// break the administrative action — but it is logged. sk = TS#<iso>#<rand>
// guarantees temporal ordering and uniqueness; ttl expires in 90 days.
func writeAudit(ctx context.Context, org, actor, actorRole, action, target, detail string) {
	if auditTable == "" || org == "" {
		return
	}
	now := time.Now().UTC()
	e := govcore.NewAuditEntry(org, actor, actorRole, action, target, detail, now.Format(time.RFC3339))
	sk := "TS#" + now.Format(time.RFC3339Nano) + "#" + strconv.FormatInt(now.UnixNano()%100000, 10)
	ttl := strconv.FormatInt(now.Add(90*24*time.Hour).Unix(), 10)
	_, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: &auditTable, Item: map[string]ddbtypes.AttributeValue{
		"org":        &ddbtypes.AttributeValueMemberS{Value: org},
		"sk":         &ddbtypes.AttributeValueMemberS{Value: sk},
		"actor":      &ddbtypes.AttributeValueMemberS{Value: e.Actor},
		"actor_role": &ddbtypes.AttributeValueMemberS{Value: e.ActorRole},
		"action":     &ddbtypes.AttributeValueMemberS{Value: e.Action},
		"target":     &ddbtypes.AttributeValueMemberS{Value: e.Target},
		"detail":     &ddbtypes.AttributeValueMemberS{Value: e.Detail},
		"ts":         &ddbtypes.AttributeValueMemberS{Value: e.TS},
		"ttl":        &ddbtypes.AttributeValueMemberN{Value: ttl},
	}})
	if err != nil {
		// Best-effort: auditing must not break the operation. Structured log.
		os.Stderr.WriteString(`{"level":"warn","msg":"audit write failed","err":"` + err.Error() + `"}` + "\n")
	}

	// ALSO feeds the Audit domain's trail, which is where auditing now lives:
	// a store separate from the audited domain, append-only by IAM, with
	// category, diff, origin and per-plan retention.
	//
	// Why both for now: the "Settings → Audit" screen still reads the legacy
	// table, and turning this write off would leave it silently empty — which the
	// front-end steering forbids. The legacy write goes away when that screen is
	// replaced by the Logs sub-tabs. This is a transition, not the final design.
	emitAuditDetail(ctx, curAudit, action, org, "", target, detail, nil)
}

// listAudit reads the org's most recent audit records (Query by pk, descending
// order by sk).
func listAudit(ctx context.Context, org string, limit int32) ([]map[string]interface{}, error) {
	if auditTable == "" {
		return nil, nil
	}
	out, err := ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              &auditTable,
		KeyConditionExpression: aws.String("org = :o"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":o": &ddbtypes.AttributeValueMemberS{Value: org},
		},
		ScanIndexForward: aws.Bool(false),
		Limit:            aws.Int32(limit),
	})
	if err != nil {
		return nil, err
	}
	var rows []map[string]interface{}
	for _, it := range out.Items {
		row := map[string]interface{}{}
		for _, k := range []string{"actor", "actor_role", "action", "target", "detail", "ts"} {
			if v, ok := it[k].(*ddbtypes.AttributeValueMemberS); ok {
				row[k] = v.Value
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// memberOrg reads a user's custom:org_id by e-mail (ownership check).
func memberOrg(ctx context.Context, email string) (string, bool) {
	org, ok, _ := identity.GetUserOrg(ctx, email)
	return org, ok
}

// ownerStats: the target's current role and how many owners the org has ("last
// owner" safeguard — the org can never be left without an owner).
func ownerStats(ctx context.Context, org, email string) (targetRole string, owners int) {
	mem, _ := listMembers(ctx, org)
	for _, m := range mem {
		if m.Role == "owner" {
			owners++
		}
		if strings.EqualFold(m.Email, email) {
			targetRole = m.Role
		}
	}
	return
}

// teamErrStatus maps the team/app domain's sentinel errors to HTTP statuses.
func teamErrStatus(err error) int {
	switch err {
	case govcore.ErrExists:
		return 409
	case govcore.ErrNotFound, govcore.ErrTeamNotFound:
		return 404
	case govcore.ErrHasApps:
		return 409
	case govcore.ErrInvalidName:
		return 400
	default:
		return 500
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func resp(status int, obj interface{}, origin string) (events.APIGatewayProxyResponse, error) {
	b, _ := json.Marshal(obj)
	return events.APIGatewayProxyResponse{StatusCode: status, Headers: getCORSHeaders(origin), Body: string(b)}, nil
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

// allClaims returns every string claim, for callers (NewAuthContext) that
// consume the whole set instead of one key at a time.
func allClaims(req events.APIGatewayProxyRequest) map[string]string {
	out := map[string]string{}
	if req.RequestContext.Authorizer == nil {
		return out
	}
	c, ok := req.RequestContext.Authorizer["claims"].(map[string]interface{})
	if !ok {
		return out
	}
	for k, v := range c {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// --- RBAC: a shell over the pure domain (internal/govcore) ---
//
// The role/scope logic lives in govcore (role matrix covered by a property
// test). These functions are the thin translation layer used by the handler and
// by the characterization tests. Behavior is
// identical to the original closures — the characterization proves it.

type access struct {
	isPlatform bool
	canAdmin   bool
	teamScoped bool
}

func (a access) gc() govcore.Access {
	return govcore.Access{IsPlatform: a.isPlatform, CanAdmin: a.canAdmin, TeamScoped: a.teamScoped}
}

// resolveAccess derives the access verdict from the JWT claims.
func resolveAccess(claimOrg, role, claimTeam string) access {
	a := govcore.Resolve(govcore.Claims{Org: claimOrg, Role: role, Team: claimTeam})
	return access{isPlatform: a.IsPlatform, canAdmin: a.CanAdmin, teamScoped: a.TeamScoped}
}

// forceOrgFor guarantees an org user only operates within its own scope.
func forceOrgFor(a access, claimOrg, param string) (string, bool) {
	return govcore.ForceOrg(a.gc(), govcore.Claims{Org: claimOrg}, param)
}

// effTeamFor forces the claim's team for team-scoped users; otherwise it honors
// the parameter.
func effTeamFor(a access, claimTeam, param string) string {
	return govcore.EffTeam(a.gc(), govcore.Claims{Team: claimTeam}, param)
}

// --- Config scopes: a shell over govcore (progressive hierarchy) ---

// scopeKey builds the key of the MOST specific scope provided.
func scopeKey(org, team, app string) string { return govcore.ScopeKey(org, team, app) }

// scopeKeys returns the inheritance chain, from least to most specific.
func scopeKeys(org, team, app string) []string { return govcore.ScopeKeys(org, team, app) }

// creditSpend reads the accumulated credit consumption per org+provider from the
// Core's counter. Returns 0 when there is no counter (nothing consumed yet, or
// the table is unavailable) — it never fails the response over that: the balance
// is an estimate, and a missing read must not break the credit screen.
func creditSpend(ctx context.Context, org, provider string) float64 {
	if limitsTable == "" {
		return 0
	}
	out, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &limitsTable,
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: "CREDIT#" + org + "#" + provider},
		},
	})
	if err != nil || out.Item == nil {
		return 0
	}
	if v, ok := out.Item["spend"].(*ddbtypes.AttributeValueMemberN); ok {
		f, _ := strconv.ParseFloat(v.Value, 64)
		return f
	}
	return 0
}

// writeOrgCredits stores the credits map in the org's config while preserving the
// rest of the document (read-modify-write). Credit always lives in the ORG# scope:
// the credit contract is with the company, not with a team or app.
func writeOrgCredits(ctx context.Context, org string, credits map[string]interface{}) error {
	key := scopeKey(org, "", "")
	cfg := readScope(ctx, key)
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	for k := range cfg {
		if strings.HasPrefix(k, "_") {
			delete(cfg, k)
		}
	}
	if len(credits) == 0 {
		delete(cfg, "credits")
	} else {
		cfg["credits"] = credits
	}
	b, _ := json.Marshal(cfg)
	_, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: &configTable, Item: map[string]ddbtypes.AttributeValue{
		"pk":     &ddbtypes.AttributeValueMemberS{Value: key},
		"config": &ddbtypes.AttributeValueMemberS{Value: string(b)},
	}})
	return err
}

// orgCredits returns the raw map of credits declared in the org's scope.
func orgCredits(ctx context.Context, org string) map[string]interface{} {
	cfg := readScope(ctx, scopeKey(org, "", ""))
	if cfg == nil {
		return map[string]interface{}{}
	}
	if m, ok := cfg["credits"].(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func readScope(ctx context.Context, key string) map[string]interface{} {
	out, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &configTable,
		Key:       map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: key}},
	})
	if err != nil || out.Item == nil {
		return nil
	}
	v, ok := out.Item["config"].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return nil
	}
	var m map[string]interface{}
	if json.Unmarshal([]byte(v.Value), &m) != nil {
		return nil
	}
	return m
}

// deepMerge: a shell over govcore.DeepMerge (maps merge by key; scalars and
// lists replace).
func deepMerge(dst, src map[string]interface{}) { govcore.DeepMerge(dst, src) }

// putSecret applies the platform prefix (aiplat/gateway/) when absent and writes
// through the port. The naming/scope policy belongs to the shell; the write
// belongs to the adapter.
func putSecret(ctx context.Context, name, apiKey string) (string, error) {
	safe := name
	if !strings.HasPrefix(safe, secretPrefix) {
		safe = secretPrefix + name
	}
	return secrets.Put(ctx, safe, apiKey)
}

// putSecretRaw writes under the exact name provided (used by the per-org BYO
// scope).
func putSecretRaw(ctx context.Context, name, apiKey string) (string, error) {
	return secrets.Put(ctx, name, apiKey)
}

// bedrockModel is one INVOCABLE Bedrock target. ModelID is the string that goes
// into the route's provider_model_id, so it is either a cross-region inference
// profile id (us.anthropic.claude-sonnet-5) or a bare foundation model id
// (mistral.mistral-7b-instruct-v0:2) — see listBedrockModels for why both spaces
// have to be listed together.
type bedrockModel struct {
	ModelID      string `json:"model_id"`
	ModelName    string `json:"model_name"`
	Provider     string `json:"provider"`
	InputModes   string `json:"input_modes"`
	OutputModes  string `json:"output_modes"`
	Customizable bool   `json:"customizable"`
	// Kind tells the console which ID space this came from: "inference_profile"
	// or "foundation_model". Without it the two are indistinguishable in the
	// dropdown, and picking the wrong one fails only at invocation time.
	Kind string `json:"kind"`
	// BaseModelID is the foundation model behind an inference profile (empty for
	// a foundation model). It is what a price table keyed on the bare id matches on.
	BaseModelID string `json:"base_model_id,omitempty"`
	// Streaming mirrors responseStreamingSupported on the foundation model.
	Streaming bool `json:"streaming"`
}

// modelModes joins a modality list into a comma-separated string.
func modelModes[T ~string](in []T) string {
	if len(in) == 0 {
		return ""
	}
	out := make([]string, 0, len(in))
	for _, m := range in {
		out = append(out, string(m))
	}
	return strings.Join(out, ",")
}

// baseModelFromARN reduces a foundation-model ARN to its bare model id.
// arn:aws:bedrock:us-west-2::foundation-model/anthropic.claude-sonnet-5 → anthropic.claude-sonnet-5
func baseModelFromARN(arn string) string {
	if i := strings.Index(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

// vendorFromModelID derives the vendor from the id, for the models that have an
// inference profile but are NOT returned by ListFoundationModels at all (measured:
// 7 in us-west-2, including Nova Premier). Those carry no providerName, and an empty
// vendor puts a first-party model in an "other" bucket next to its own siblings.
// us.amazon.nova-premier-v1:0 → Amazon.
func vendorFromModelID(id string) string {
	s := id
	for _, p := range []string{"us-gov.", "us.", "eu.", "apac.", "global."} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimPrefix(s, p)
			break
		}
	}
	v := s
	if i := strings.Index(v, "."); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return ""
	}
	return strings.ToUpper(v[:1]) + v[1:]
}

// orgDeclaresBaseURL reports whether baseURL (case-insensitive, trailing slash ignored)
// is the base_url of at least one route saved at the org scope. It is the allowlist for
// the outbound credential-bearing call in /admin/provider/models: without it, an
// otherwise-legitimate admin request could redirect the org's own provider credential to
// any HTTPS host by naming it in base_url.
func orgDeclaresBaseURL(ctx context.Context, org, baseURL string) bool {
	m := readScope(ctx, scopeKey(org, "", ""))
	routing, _ := m["routing"].(map[string]interface{})
	return baseURLDeclaredIn(routing, baseURL)
}

// gatewayRoleARNRe is the shape an assumable gateway role must have: the AIPlatGatewayAccess
// role (optionally suffixed) in a 12-digit account. It is the FORM check; orgDeclaresRoleARN
// is the allowlist check. Both must pass before the platform assumes a caller-named role
// (GOV-SEC-002) — otherwise GET /admin/bedrock/models is an oracle for probing which
// accounts trust the platform role, driven by any member.
var gatewayRoleARNRe = regexp.MustCompile(`^arn:aws:iam::\d{12}:role/AIPlatGatewayAccess[\w+=,.@-]*$`)

// orgDeclaresRoleARN reports whether roleARN is the role_arn of at least one route the org
// saved at the org scope — the same allowlist idea as orgDeclaresBaseURL, applied to the
// cross-account AssumeRole target.
func orgDeclaresRoleARN(ctx context.Context, org, roleARN string) bool {
	m := readScope(ctx, scopeKey(org, "", ""))
	routing, _ := m["routing"].(map[string]interface{})
	return roleARNDeclaredIn(routing, roleARN)
}

// roleARNDeclaredIn is the pure comparison (exact match) orgDeclaresRoleARN applies.
func roleARNDeclaredIn(routing map[string]interface{}, roleARN string) bool {
	for _, r := range routing {
		rm, _ := r.(map[string]interface{})
		if ra, _ := rm["role_arn"].(string); ra != "" && ra == roleARN {
			return true
		}
	}
	return false
}

// validateProviderHost rejects a base_url whose host is (or resolves to) a loopback,
// private, link-local or unspecified address — the SSRF guard for the credential-bearing
// provider-model call (GOV-SEC-006). The Lambda runs outside a VPC, so the blast radius is
// the public internet plus these ranges; refusing them removes the internal-host reach.
func validateProviderHost(rawURL string) error {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Host == "" {
		return errors.New("invalid base_url")
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return errors.New("base_url host is not allowed")
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		addrs, lerr := net.LookupIP(host)
		if lerr != nil {
			return errors.New("base_url host does not resolve")
		}
		ips = addrs
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return errors.New("base_url resolves to a private or loopback address")
		}
	}
	return nil
}

// validateWebhookURL is the WRITE-time guard on an alert webhook (OBS-SEC-001): https
// only, no localhost, and no LITERAL loopback/private/link-local/unspecified IP.
//
// OBS-NFR-005: it deliberately does NOT resolve DNS. Resolving at write time made a save
// fail with a 400 the moment a host stopped resolving (e.g. the demo fixture
// hooks.example.com), and made the unit tests depend on outbound DNS in CI. The SSRF that
// DNS resolution defends against matters at DELIVERY time, not at save time — the
// alert-notifier re-resolves right before each POST (and thereby also defeats a rebind
// between save and delivery). So the write side checks only what it can know statically;
// the delivery side owns the resolving guard.
func validateWebhookURL(rawURL string) error {
	u, err := neturl.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return errors.New("invalid webhook_url")
	}
	if u.Scheme != "https" {
		return errors.New("webhook_url must use https")
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return errors.New("webhook_url host is not allowed")
	}
	// Only reject a LITERAL non-routable IP. A hostname is left for the delivery-time
	// resolver to vet, so an unresolvable-but-syntactically-valid host does not block a save.
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return errors.New("webhook_url is a private or loopback address")
		}
	}
	return nil
}

// webhookURLFromConfig extracts alerts.webhook_url from a config body, returning "" when
// the write does not touch it (so an unrelated PUT is not forced to re-validate).
func webhookURLFromConfig(cfg map[string]interface{}) string {
	alerts, ok := cfg["alerts"].(map[string]interface{})
	if !ok {
		return ""
	}
	if w, ok := alerts["webhook_url"].(string); ok {
		return strings.TrimSpace(w)
	}
	return ""
}

// baseURLDeclaredIn is the pure comparison orgDeclaresBaseURL applies to a routing map —
// split out so the matching rule (case-insensitive, trailing slash ignored) is testable
// without DynamoDB.
func baseURLDeclaredIn(routing map[string]interface{}, baseURL string) bool {
	norm := func(u string) string { return strings.TrimRight(strings.ToLower(strings.TrimSpace(u)), "/") }
	want := norm(baseURL)
	for _, r := range routing {
		rm, _ := r.(map[string]interface{})
		if bu, _ := rm["base_url"].(string); bu != "" && norm(bu) == want {
			return true
		}
	}
	return false
}

// listProviderModels lists an external/self-hosted provider's models using the org's
// credential kept in the vault — the key NEVER goes back to the client. Covers
// the OpenAI dialect (GET {base}/models: OpenAI, Groq, xAI, Azure, self-hosted…),
// Anthropic (x-api-key + /v1/models) and Google Gemini (/v1beta/models?key=).
// base_url is the address declared by the customer (the same destination the
// gateway already calls at runtime); short timeout and bounded body.
func listProviderModels(ctx context.Context, adapter, baseURL, key string) ([]string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, errors.New("base_url is empty")
	}
	// The provider credential travels on this request. Refuse a cleartext
	// endpoint: over http:// the key (and the prompt, at runtime) crosses the
	// network readable by anything on the path. This is also why a self-hosted
	// endpoint must terminate TLS — the gateway runs outside a VPC, so "internal
	// address" is not a substitute for encryption here.
	if !strings.HasPrefix(strings.ToLower(base), "https://") {
		return nil, errors.New("base_url must use https:// — a cleartext endpoint would expose the provider key in transit")
	}
	// GOV-SEC-006: reject a base_url that points at a private/loopback/link-local
	// address before the credential-bearing request leaves.
	if err := validateProviderHost(base); err != nil {
		return nil, err
	}
	var req *http.Request
	switch adapter {
	case "google", "gemini":
		// GOV-SEC-006: the Gemini key goes in the x-goog-api-key HEADER, not the URL
		// query — a URL carries into logs, referrers and redirect targets.
		req, _ = http.NewRequestWithContext(ctx, "GET", base+"/v1beta/models", nil)
		req.Header.Set("x-goog-api-key", key)
	case "anthropic":
		req, _ = http.NewRequestWithContext(ctx, "GET", base+"/v1/models", nil)
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	default: // openai_compatible and similar
		req, _ = http.NewRequestWithContext(ctx, "GET", base+"/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, errors.New("provider responded " + strconv.Itoa(resp.StatusCode))
	}
	var ids []string
	if adapter == "google" || adapter == "gemini" {
		var gg struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		json.Unmarshal(body, &gg)
		for _, m := range gg.Models {
			ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
		}
	} else {
		var oa struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		json.Unmarshal(body, &oa)
		for _, m := range oa.Data {
			if m.ID != "" {
				ids = append(ids, m.ID)
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// listBedrockModels lists the Bedrock targets that can actually be invoked in the
// customer's account via AssumeRole. When roleARN is empty it uses the platform
// account.
//
// Two APIs, because Bedrock has TWO ID spaces and neither one alone is the answer:
//
//   - ListFoundationModels returns bare ids (anthropic.claude-sonnet-5). Most of the
//     current generation is INFERENCE_PROFILE-only, so the bare id is NOT invocable —
//     Converse answers 404 / "end of life". Measured in us-west-2: 43 of 108 models
//     are in that state, and they are the interesting ones (every Claude, Nova Pro,
//     Llama 3.3/4, DeepSeek R1, Pixtral).
//   - ListInferenceProfiles returns the cross-region ids (us.anthropic.claude-sonnet-5)
//     that DO resolve, but carries no modality or streaming metadata.
//
// So: index every foundation model (no filter — the index is metadata, not the
// answer), emit one entry per ACTIVE SYSTEM_DEFINED profile enriched from that index,
// then add the foundation models that support ON_DEMAND and are not already covered
// by a profile. Listing only one space is what made the console show ~12 models.
//
// A missing ListInferenceProfiles permission is NOT fatal: an older cross-account
// role granted before this change would otherwise stop listing anything at all, so
// the profile half degrades to empty and the foundation-model half still answers.
func listBedrockModels(ctx context.Context, roleARN, externalID, region string) ([]bedrockModel, error) {
	if region == "" {
		region = "us-east-1"
	}

	var cfg aws.Config
	var err error

	if roleARN != "" {
		// AssumeRole in the customer's account
		cfg, err = awscfg.LoadDefaultConfig(ctx, awscfg.WithRegion(region))
		if err != nil {
			return nil, err
		}
		creds := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), roleARN, func(o *stscreds.AssumeRoleOptions) {
			if externalID != "" {
				o.ExternalID = &externalID
			}
		})
		cfg.Credentials = aws.NewCredentialsCache(creds)
	} else {
		// Platform account (pooled)
		cfg, err = awscfg.LoadDefaultConfig(ctx, awscfg.WithRegion(region))
		if err != nil {
			return nil, err
		}
	}

	client := bedrock.NewFromConfig(cfg)
	out, err := client.ListFoundationModels(ctx, &bedrock.ListFoundationModelsInput{})
	if err != nil {
		return nil, err
	}

	// Index by bare model id. Deliberately unfiltered: this is the metadata source
	// for the profile half, and an INFERENCE_PROFILE-only model has to be findable
	// here precisely because it is not invocable on its own.
	type fmEntry struct {
		name, provider, in, out string
		customizable, streaming bool
		onDemand                bool
	}
	index := make(map[string]fmEntry, len(out.ModelSummaries))
	for _, m := range out.ModelSummaries {
		e := fmEntry{
			name:         aws.ToString(m.ModelName),
			provider:     aws.ToString(m.ProviderName),
			in:           modelModes(m.InputModalities),
			out:          modelModes(m.OutputModalities),
			customizable: len(m.CustomizationsSupported) > 0,
			streaming:    aws.ToBool(m.ResponseStreamingSupported),
		}
		for _, t := range m.InferenceTypesSupported {
			if t == bedrocktypes.InferenceTypeOnDemand {
				e.onDemand = true
			}
		}
		index[aws.ToString(m.ModelId)] = e
	}

	// textOnly keeps the dropdown to what this gateway can actually route. It is a
	// chat/completions gateway, so an embedding or image model is noise the customer
	// would have to recognise and skip. An UNDECLARED modality passes: refusing it
	// would silently hide a brand-new model, which is the failure we are fixing.
	textOnly := func(modes string) bool {
		return modes == "" || strings.Contains(modes, "TEXT")
	}

	var models []bedrockModel
	covered := map[string]bool{} // bare ids already reachable through a profile

	// ---- Half 1: cross-region inference profiles (the invocable ids) ----
	var token *string
	for page := 0; page < 20; page++ { // bounded: a runaway NextToken must not hang the request
		in := &bedrock.ListInferenceProfilesInput{
			TypeEquals: bedrocktypes.InferenceProfileTypeSystemDefined,
			MaxResults: aws.Int32(100),
			NextToken:  token,
		}
		pr, perr := client.ListInferenceProfiles(ctx, in)
		if perr != nil {
			// Degrade instead of failing the whole listing (see the doc comment).
			os.Stderr.WriteString(`{"level":"warn","msg":"list inference profiles failed","err":"` + perr.Error() + `"}` + "\n")
			break
		}
		for _, p := range pr.InferenceProfileSummaries {
			if p.Status != bedrocktypes.InferenceProfileStatusActive {
				continue
			}
			id := aws.ToString(p.InferenceProfileId)
			if id == "" {
				continue
			}
			base := ""
			if len(p.Models) > 0 {
				base = baseModelFromARN(aws.ToString(p.Models[0].ModelArn))
			}
			meta := index[base]
			if !textOnly(meta.out) {
				continue
			}
			covered[base] = true
			name := meta.name
			if name == "" {
				name = aws.ToString(p.InferenceProfileName)
			}
			provider := meta.provider
			if provider == "" {
				provider = vendorFromModelID(id)
			}
			models = append(models, bedrockModel{
				ModelID:      id,
				ModelName:    name,
				Provider:     provider,
				InputModes:   meta.in,
				OutputModes:  meta.out,
				Customizable: meta.customizable,
				Kind:         "inference_profile",
				BaseModelID:  base,
				Streaming:    meta.streaming,
			})
		}
		token = pr.NextToken
		if token == nil {
			break
		}
	}

	// ---- Half 2: foundation models invocable directly (ON_DEMAND) ----
	for id, e := range index {
		if !e.onDemand || covered[id] || !textOnly(e.out) {
			continue
		}
		models = append(models, bedrockModel{
			ModelID:      id,
			ModelName:    e.name,
			Provider:     e.provider,
			InputModes:   e.in,
			OutputModes:  e.out,
			Customizable: e.customizable,
			Kind:         "foundation_model",
			Streaming:    e.streaming,
		})
	}

	// Stable order: the map iteration above is random, and an unstable dropdown
	// looks like the list changed between two fetches.
	sort.Slice(models, func(i, j int) bool {
		if models[i].Provider != models[j].Provider {
			return models[i].Provider < models[j].Provider
		}
		return models[i].ModelID < models[j].ModelID
	})
	return models, nil
}

func handle(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	method := req.HTTPMethod
	path := req.Path

	// Extract origin from request headers
	origin := req.Headers["origin"]
	if origin == "" {
		origin = req.Headers["Origin"] // Try capitalized version
	}

	// Handle preflight requests
	if method == "OPTIONS" {
		return events.APIGatewayProxyResponse{StatusCode: 204, Headers: getCORSHeaders(origin)}, nil
	}

	// RATE LIMITING - Check before authentication to prevent brute force attacks
	sourceIP := req.RequestContext.Identity.SourceIP
	identifier := hashIdentifier(sourceIP)

	// Allow 100 requests per minute per IP
	allowed, err := rateLimiter.CheckRateLimit(ctx, identifier, 100, time.Minute)
	if err != nil {
		// Log error but don't block request if rate limiting fails
		os.Stderr.WriteString(`{"level":"warn","msg":"rate limit check failed","err":"` + err.Error() + `"}` + "\n")
	} else if !allowed {
		// Return 429 Too Many Requests with proper CORS headers
		headers := getCORSHeaders(origin)
		headers["retry-after"] = "60"
		return events.APIGatewayProxyResponse{
			StatusCode: 429,
			Headers:    headers,
			Body:       `{"error":"rate limit exceeded","retry_after":60}`,
		}, nil
	}

	// Isolation: the org comes from the Cognito CLAIMS (validated by API Gateway).
	claimOrg := claim(req, "custom:org_id")
	role := claim(req, "custom:role")
	claimTeam := claim(req, "team")
	claimEmail := claim(req, "email")
	// Role/scope resolution. The logic is PURE and lives in package-level
	// functions (resolveAccess/forceOrgFor/effTeamFor) so it can be covered by
	// characterization tests before moving to internal/govcore
	//.
	// Audit context: the actor and the origin of the REQUEST. Built once and
	// passed along, so no emission site needs to reach the request object — which
	// would open the door to deriving authorship from client input.
	aud := auditCtx{
		actor:     newAuditActor(claimEmail, claim(req, "sub"), role),
		sourceIP:  req.RequestContext.Identity.SourceIP,
		userAgent: req.RequestContext.Identity.UserAgent,
	}
	curAudit = aud

	acc := resolveAccess(claimOrg, role, claimTeam)
	isPlatform := acc.isPlatform
	teamScoped := acc.teamScoped
	canAdmin := acc.canAdmin
	// effTeam forces the claim's team for team-scoped users (ignoring the
	// parameter); for everyone else it honors what came in.
	effTeam := func(param string) string { return effTeamFor(acc, claimTeam, param) }
	// forceOrg guarantees an org user only operates within its own scope.
	// The "global" scope (it affects the whole platform, not just one
	// team/app) is exclusive to platform_admin.
	forceOrg := func(param string) (string, bool) { return forceOrgFor(acc, claimOrg, param) }

	// Create authorization context from Cognito claims
	authCtx := NewAuthContext(allClaims(req))

	switch {
	case strings.HasSuffix(path, "/admin/config") && method == "GET":
		q := req.QueryStringParameters
		gorg, okScope := forceOrg(q["org"])
		if !okScope {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(gorg) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_access_attempt", fmt.Sprintf("org:%s", gorg), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}
		// GOV-SEC-008: reading a specific team's scope requires access to THAT team.
		// effTeam already pins a team-scoped token to its own team; this additionally
		// stops a non-admin WITHOUT a team claim (dev/billing) from reading an
		// arbitrary team's policy via ?team=. Org-level reads (no team) are unaffected.
		if reqTeam := effTeam(q["team"]); reqTeam != "" && !authCtx.CanAccessTeam(gorg, reqTeam) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_team_config_read_attempt", fmt.Sprintf("org:%s,team:%s", gorg, reqTeam), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}
		// ?effective=1 returns the resolved config (global → ORG → TEAM → APP),
		// which is exactly what the Core applies at runtime.
		// The config always comes back at the ROOT of the JSON (console compat),
		// with scope metadata under "_scope".
		if q["effective"] == "1" || q["effective"] == "true" {
			chain := scopeKeys(gorg, effTeam(q["team"]), q["app"])
			merged := map[string]interface{}{}
			// allowed_models resolves by INTERSECTION, not replacement, so it is
			// folded separately — deepMerge would let a child widen its parent here
			// while the gateway (which intersects) refuses the extra models, and this
			// endpoint's whole contract is to be "what the Core applies at runtime".
			//
			// The ceiling is carried in a LOCAL accumulator instead of being read back
			// out of `merged` each round. Round-tripping it through the map looks
			// equivalent and is not: the resolved value is a []string while a
			// freshly-decoded document holds []interface{}, so the next read's type
			// assertion failed and the ceiling silently reset to the last level that
			// declared one. Measured against the live API, not caught by the contract
			// test — which drives the pure rule and never touches this plumbing.
			var allowedAcc []string
			for _, k := range chain {
				if m := readScope(ctx, k); m != nil {
					child := allowedFrom(m)
					deepMerge(merged, m)
					allowedAcc = govcore.IntersectAllowed(allowedAcc, child)
				}
			}
			// deepMerge left the last declaring level's raw list in place; replace it
			// with the resolved ceiling, or drop the key when nothing declared one.
			if allowedAcc != nil {
				merged["allowed_models"] = allowedAcc
			} else {
				delete(merged, "allowed_models")
			}
			merged["_scope"] = chain
			merged["_effective"] = true
			return resp(200, merged, origin)
		}
		// Without ?effective: returns the RAW config of that scope (what is set there).
		key := scopeKey(gorg, effTeam(q["team"]), q["app"])
		m := readScope(ctx, key)
		if m == nil {
			m = map[string]interface{}{}
		}
		m["_scope"] = key
		return resp(200, m, origin)

	case strings.HasSuffix(path, "/admin/config") && method == "PUT":
		// Role: only owner/admin change policy (routing, budget, guardrails, models).
		if !canAdmin {
			return resp(403, map[string]string{"error": "your role cannot change the configuration (owner/admin only)"}, origin)
		}
		var cfg map[string]interface{}
		if err := ReadAndValidateJSON(req.Body, &cfg); err != nil {
			return resp(400, map[string]interface{}{"error": err.Error()}, origin)
		}
		// Compat: the old console sends the config at the root of the body. When
		// {"scope":{...},"config":{...}} comes in, use that format instead.
		q := req.QueryStringParameters
		org, team, app := q["org"], q["team"], q["app"]
		if inner, ok := cfg["config"].(map[string]interface{}); ok {
			if sc, ok2 := cfg["scope"].(map[string]interface{}); ok2 {
				str := func(k string) string {
					if v, ok := sc[k].(string); ok {
						return v
					}
					return ""
				}
				org, team, app = str("org"), str("team"), str("app")
			}
			cfg = inner
		}
		// Scope forced by the token: an org user writes neither into another org
		// nor into the "global" scope (which would affect the whole platform).
		org, okScope := forceOrg(org)
		if !okScope {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_config_modification_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}
		// A team-scoped user only writes to its own team (never org/global): force
		// the claim's team, which guarantees a pk always carrying TEAM#.
		team = effTeam(team)

		// CHECK AUTHORIZATION: Verify user can access the requested team
		if team != "" && !authCtx.CanAccessTeam(org, team) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_team_config_modification_attempt", fmt.Sprintf("org:%s,team:%s", org, team), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		key := scopeKey(org, team, app)
		// Do not persist metadata returned by the GET (_scope, _effective).
		for k := range cfg {
			if strings.HasPrefix(k, "_") {
				delete(cfg, k)
			}
		}
		// OBS-SEC-001: validate an alert webhook URL at write time so an SSRF target
		// (non-https, or a host resolving to a private/loopback/link-local address) can
		// never be stored. The alert-notifier also checks at delivery; this stops it at
		// the source. Only runs when the write actually sets alerts.webhook_url.
		if wh := webhookURLFromConfig(cfg); wh != "" {
			if err := validateWebhookURL(wh); err != nil {
				return resp(400, map[string]string{"error": "invalid_webhook_url", "message": err.Error()}, origin)
			}
		}
		// GOV-SEC-001: the team/app in the write target must exist in the org
		// registry (no orphan APP#<anything> scopes).
		if team != "" && team != "default" {
			tree := readOrgTree(ctx, org)
			if _, ok := tree.Teams[team]; !ok {
				return resp(400, map[string]string{"error": "unknown_team", "message": "team does not exist in this org: " + team}, origin)
			}
			if app != "" {
				if am, ok := tree.Apps[app]; !ok || am.Team != team {
					return resp(400, map[string]string{"error": "unknown_app", "message": "app does not exist in this team: " + app}, origin)
				}
			}
		} else if app != "" {
			tree := readOrgTree(ctx, org)
			if am, ok := tree.Apps[app]; !ok || (am.Team != "" && am.Team != "default") {
				return resp(400, map[string]string{"error": "unknown_app", "message": "app does not exist: " + app}, origin)
			}
		}
		// GOV-SEC-001: a child scope (TEAM#/APP#) may restrict a parent policy, never
		// WIDEN it. Build the effective PARENT (every scope ABOVE this write target)
		// and reject any key the child loosens — the write-side mirror of the Core's
		// read-side foldCeilings, verified against the same config-scope contract.
		//
		// GOV-FUN-002: a GLOBAL write (org == "") has NO parent, so there is nothing to
		// compare it against. The old code seeded parentKeys with "global" and then
		// compared the global write to the current global item — i.e. the scope against
		// its own replacement — which turned every global policy into a one-way ratchet
		// (the platform operator could only ever tighten it). Skip the ceiling check
		// entirely for a global write. Global REMAINS a ceiling for ORG/TEAM/APP writes,
		// consistent with the Core's read-side fold (global → ORG → TEAM → APP).
		// parentEff stays visible to the audit emission below (auditParentModels). It
		// is nil for a global write, which is harmless there because the whole audit
		// block is org-only.
		var parentEff map[string]interface{}
		if org != "" {
			belowOrg := team != "" || app != ""
			parentKeys := []string{"global"}
			if belowOrg {
				parentKeys = append(parentKeys, "ORG#"+org)
				if app != "" {
					pt := team
					if pt == "" {
						pt = "default"
					}
					parentKeys = append(parentKeys, "ORG#"+org+"#TEAM#"+pt)
				}
			}
			parentEff = map[string]interface{}{}
			var parentAllowed []string
			for _, pk := range parentKeys {
				if m := readScope(ctx, pk); m != nil {
					parentAllowed = govcore.IntersectAllowed(parentAllowed, allowedFrom(m))
					deepMerge(parentEff, m)
				}
			}
			if parentAllowed != nil {
				parentEff["allowed_models"] = parentAllowed
			}
			if viol := govcore.CeilingViolations(parentEff, cfg, belowOrg); len(viol) > 0 {
				writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
					"config_scope_widening_rejected", key, strings.Join(viol, ","))
				return resp(400, map[string]interface{}{"error": "scope_widening", "keys": viol,
					"message": "a team/app scope cannot widen a parent policy: " + strings.Join(viol, ", ")}, origin)
			}
		}
		// Audit: read the PREVIOUS state of that scope before overwriting.
		// Cheap (the same read the merge already does on other paths) and it is
		// what allows recording the field-by-field diff — without the "before",
		// the trail says something changed but not against what.
		prev := readScope(ctx, key)
		if prev == nil {
			prev = map[string]interface{}{}
		}

		b, _ := json.Marshal(cfg)
		_, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: &configTable, Item: map[string]ddbtypes.AttributeValue{
			"pk":     &ddbtypes.AttributeValueMemberS{Value: key},
			"config": &ddbtypes.AttributeValueMemberS{Value: string(b)},
		}})
		if err != nil {
			return resp(500, map[string]string{"error": err.Error()}, origin)
		}

		// Emission comes AFTER the write succeeded: auditing a change that never
		// happened would be worse than not auditing at all. And it must not fail
		// the response — emitAudit swallows any error.
		if org != "" {
			changes := auditDiff(prev, cfg)
			// Model changes go out as their OWN events (model_add/update/remove)
			// because there is no model route: the Console writes models inside
			// "routing". Without this, "who registered this model?" would have no
			// answer.
			// GOV-UX-002: an alias that exists in a parent scope is being OVERRIDDEN
			// here, not added/removed — classify those as model_update so the trail
			// does not read as if the team deleted a platform model.
			parentModels := auditParentModels(parentEff)
			for model, chs := range auditDeriveModels(changes) {
				emitAudit(ctx, aud, auditClassifyModelScoped(chs, parentModels[model]), org, key, model, chs)
			}
			// Bundles go out as their own events for the SAME reason as models: a
			// bundle is the attempt order of a critical flow, and "who changed the
			// order of the reasoning?" must have a direct answer, not a line lost
			// inside a thirty-field config_update.
			for bundle, chs := range auditDeriveBundles(changes) {
				emitAudit(ctx, aud, auditClassifyBundle(chs), org, key, bundle, chs)
			}
			// The swap policy is the feature's quality promise. Loosening it from
			// same_model_only to allow_downgrade changes what the customer receives
			// without changing a line of their code — exactly the kind of change
			// that needs an actor, a timestamp and the previous value.
			for feature, chs := range auditDeriveSwapPolicy(changes) {
				emitAudit(ctx, aud, audSwapPolicyUpdate, org, key, feature, chs)
			}
			// Everything else (budget, guardrails, limits, cache…) goes as config_update.
			var rest []auditChange
			for _, c := range changes {
				if !auditDerivedElsewhere(c.Path) {
					rest = append(rest, c)
				}
			}
			if len(rest) > 0 {
				emitAudit(ctx, aud, audConfigUpdate, org, key, "", rest)
			}
		}
		return resp(200, map[string]string{"status": "saved", "scope": key}, origin)

	// GET /admin/access/matrix?org= — the served model set for EVERY scope of the
	// org in ONE request (see access_matrix.go for why it exists and what it must
	// respect). Read-only: it aggregates exactly what GET /admin/config already
	// returns per scope, so it carries the same authorization as that endpoint —
	// any authenticated caller reads its own org, and a team-scoped caller is
	// filtered to its own team. Gating this on canAdmin would be stricter than the
	// endpoint it aggregates, which would only push the console back to N calls.
	case strings.HasSuffix(path, "/admin/access/matrix") && method == "GET":
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_access_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}
		// A team-scoped caller must not read another team's policy. Without this
		// filter the aggregation would hand a dev pinned to team A the whole org's
		// configuration in a single call — the very leak the per-scope endpoint
		// avoids by forcing effTeam.
		matrixTeam := ""
		if teamScoped {
			matrixTeam = claimTeam
		}
		mx := buildAccessMatrix(ctx, readScope, org, readOrgTree(ctx, org), matrixTeam)
		return resp(200, mx, origin)

	// Provider credit (AWS Activate, Google Cloud credits, etc.).
	//
	// Credit is a BALANCE, not a discount: the price per token does not change,
	// only which pocket it comes out of. That is why the record is declared by the
	// customer (we have no provider balance API) and the consumption we show is a
	// LOWER BOUND — our counter only sees what went through the gateway, while the
	// real credit is burned across the customer's whole account (S3, EC2, Lambda).
	// The UI is required to say "estimated" and to offer a manual correction.
	case strings.HasSuffix(path, "/admin/credits") && method == "GET":
		// Read: owner/admin and billing (credit is an invoice matter). A user
		// locked to a team does not read the org scope.
		if !canAdmin && !(role == "billing" && !teamScoped) {
			return resp(403, map[string]string{"error": "your role cannot view the org credit"}, origin)
		}
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}
		if org == "" {
			return resp(400, map[string]string{"error": "credit is per org; the global scope has no credit"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_credits_access_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}
		today := time.Now().UTC()
		list := []map[string]interface{}{}
		for provider, raw := range orgCredits(ctx, org) {
			d, _ := raw.(map[string]interface{})
			if d == nil {
				continue
			}
			num := func(k string) float64 {
				f, _ := d[k].(float64)
				return f
			}
			amount, corrected := num("amount_usd"), num("corrected_remaining_usd")
			expires, _ := d["expires_at"].(string)
			consumed := creditSpend(ctx, org, provider)
			// Balance/expiry are computed in the pure domain (govcore). The
			// customer's manual correction, when present, replaces the declared
			// amount as the base.
			remaining := govcore.Remaining(amount, corrected, consumed)
			expired := govcore.Expired(expires, today)
			item := map[string]interface{}{
				"provider":      provider,
				"amount_usd":    amount,
				"consumed_usd":  consumed,
				"remaining_usd": remaining,
				"expires_at":    expires,
				"expired":       expired,
				"active":        govcore.Active(expired, remaining),
			}
			if corrected > 0 {
				item["corrected_remaining_usd"] = corrected
				item["corrected_at"], _ = d["corrected_at"].(string)
				item["corrected_by"], _ = d["corrected_by"].(string)
			}
			list = append(list, item)
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i]["provider"].(string) < list[j]["provider"].(string)
		})
		return resp(200, map[string]interface{}{
			"org":     org,
			"credits": list,
			// Explicit contract for the console: the balance is NOT exact.
			"estimated": true,
			"note":      "Balance estimated from consumption through the gateway. It is a lower bound: provider credit is also spent outside the platform. Check your provider invoice and correct it here.",
		}, origin)

	case strings.HasSuffix(path, "/admin/credits") && method == "PUT":
		if !canAdmin {
			return resp(403, map[string]string{"error": "your role cannot change credit (owner/admin only)"}, origin)
		}
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}
		if org == "" {
			return resp(400, map[string]string{"error": "credit is per org; the global scope has no credit"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_credits_modification_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}
		var b struct {
			Provider  string   `json:"provider"`
			AmountUSD *float64 `json:"amount_usd"`
			ExpiresAt *string  `json:"expires_at"`
			Corrected *float64 `json:"corrected_remaining_usd"`
		}
		if err := ReadAndValidateJSON(req.Body, &b); err != nil {
			return resp(400, map[string]interface{}{"error": err.Error()}, origin)
		}

		// Validate fields
		if errs := validateCreditsUpdate(b); len(errs) > 0 {
			return resp(400, map[string]interface{}{
				"error":   "validation failed",
				"details": errs,
			}, origin)
		}

		b.Provider = strings.ToLower(strings.TrimSpace(b.Provider))
		credits := orgCredits(ctx, org)
		entry, _ := credits[b.Provider].(map[string]interface{})
		if entry == nil {
			entry = map[string]interface{}{}
		}
		if b.AmountUSD != nil {
			entry["amount_usd"] = *b.AmountUSD
		}
		if b.ExpiresAt != nil {
			if *b.ExpiresAt == "" {
				delete(entry, "expires_at")
			} else {
				entry["expires_at"] = *b.ExpiresAt
			}
		}
		// A manual correction is audited: we record when and by whom, because it
		// overrides the number the platform computed.
		if b.Corrected != nil {
			if *b.Corrected == 0 {
				delete(entry, "corrected_remaining_usd")
				delete(entry, "corrected_at")
				delete(entry, "corrected_by")
			} else {
				entry["corrected_remaining_usd"] = *b.Corrected
				entry["corrected_at"] = time.Now().UTC().Format(time.RFC3339)
				entry["corrected_by"] = claimEmail
			}
		}
		if _, ok := entry["amount_usd"]; !ok {
			return resp(400, map[string]string{"error": "amount_usd is required when registering a new credit"}, origin)
		}
		credits[b.Provider] = entry
		if err := writeOrgCredits(ctx, org, credits); err != nil {
			return resp(500, map[string]string{"error": err.Error()}, origin)
		}
		// GOV-SEC-004: a credit mutation (balance correction, amount, expiry) is a
		// control-plane change that must be in the trail — field diff, value-free.
		var creditChs []auditChange
		if b.AmountUSD != nil {
			creditChs = append(creditChs, auditChange{Path: "amount_usd", After: *b.AmountUSD})
		}
		if b.Corrected != nil {
			creditChs = append(creditChs, auditChange{Path: "corrected_remaining_usd", After: *b.Corrected})
		}
		if b.ExpiresAt != nil {
			creditChs = append(creditChs, auditChange{Path: "expires_at", After: *b.ExpiresAt})
		}
		emitAudit(ctx, aud, audCreditUpdate, org, scopeKey(org, "", ""), b.Provider, creditChs)
		return resp(200, map[string]interface{}{"status": "saved", "provider": b.Provider, "credit": entry}, origin)

	case strings.HasSuffix(path, "/admin/credits") && method == "DELETE":
		if !canAdmin {
			return resp(403, map[string]string{"error": "your role cannot change credit (owner/admin only)"}, origin)
		}
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}
		provider := strings.ToLower(strings.TrimSpace(req.QueryStringParameters["provider"]))
		if org == "" || provider == "" {
			return resp(400, map[string]string{"error": "provider is required"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_credits_deletion_attempt", fmt.Sprintf("org:%s,provider:%s", org, provider), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}
		credits := orgCredits(ctx, org)
		if _, ok := credits[provider]; !ok {
			return resp(404, map[string]string{"error": "no credit found for that provider"}, origin)
		}
		delete(credits, provider)
		if err := writeOrgCredits(ctx, org, credits); err != nil {
			return resp(500, map[string]string{"error": err.Error()}, origin)
		}
		// GOV-SEC-004: deleting a credit record is auditable too.
		emitAudit(ctx, aud, audCreditDelete, org, scopeKey(org, "", ""), provider, nil)
		// The consumption counter is NOT deleted: it is the history of what was
		// already burned. Deleting it too would let someone zero the spend by
		// recreating the record.
		return resp(200, map[string]string{"status": "deleted", "provider": provider}, origin)

	case strings.HasSuffix(path, "/admin/secrets") && method == "POST":
		// Role: only owner/admin write provider credentials.
		if !canAdmin {
			return resp(403, map[string]string{"error": "your role cannot store credentials (owner/admin only)"}, origin)
		}
		var b struct {
			Name     string `json:"name"`
			APIKey   string `json:"api_key"`
			Value    string `json:"value"`    // alias for api_key (new console)
			Provider string `json:"provider"` // BYO: writes a per-org credential
			Org      string `json:"org"`
		}
		if err := ReadAndValidateJSON(req.Body, &b); err != nil {
			return resp(400, map[string]interface{}{"error": err.Error()}, origin)
		}

		// Validate fields
		if errs := validateSecretRequest(b); len(errs) > 0 {
			return resp(400, map[string]interface{}{
				"error":   "validation failed",
				"details": errs,
			}, origin)
		}

		key := b.APIKey
		if key == "" {
			key = b.Value
		}
		// BYO credential: when "provider" is present, write to
		// aiplat/org/<org>/<provider>. The org ALWAYS comes from the claims (a
		// regular user does not pick another org).
		var name string
		var err error
		if b.Provider != "" {
			// GOV-FUN-001: normalise the provider to lower-case on WRITE, because the
			// read path (listProviderModels) lower-cases it — a mixed-case "OpenAI"
			// stored verbatim could never be found again.
			provider := strings.ToLower(strings.TrimSpace(b.Provider))
			org, okScope := forceOrg(b.Org)
			if !okScope {
				return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
			}

			// CHECK AUTHORIZATION: Verify user can access the requested org
			if !authCtx.CanAccessOrg(org) {
				writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
					"unauthorized_secret_write_attempt", fmt.Sprintf("org:%s,provider:%s", org, provider), "")
				return resp(403, map[string]string{"error": "access denied"}, origin)
			}

			name, err = putSecretRaw(ctx, "aiplat/org/"+org+"/"+provider, key)
			if err == nil {
				// GOV-SEC-004: rotating a provider key is the most sensitive mutation —
				// it must land in the trail (the action constant existed but was never
				// emitted). The value is never included.
				emitAudit(ctx, aud, audProviderSecretSet, org, "aiplat/org/"+org+"/"+provider, provider, nil)
			}
		} else {
			if b.Name == "" {
				return resp(400, map[string]string{"error": "name is required"}, origin)
			}
			// Shared platform scope (aiplat/gateway/*): platform_admin only.
			if !isPlatform {
				return resp(403, map[string]string{"error": "a platform secret requires platform_admin; use provider for your org's credential"}, origin)
			}
			// GOV-FUN-001: store the MERGED key (api_key or its `value` alias), not the
			// raw b.APIKey — the new console sends `value`, which was being stored empty.
			name, err = putSecret(ctx, b.Name, key)
		}
		if err != nil {
			return resp(500, map[string]string{"error": err.Error()}, origin)
		}
		return resp(200, map[string]string{"secret_name": name}, origin)

	// ---- Members & Access (RBAC on top of Cognito) ----
	// The org ALWAYS comes from the token; the query string never decides whose
	// team it is.
	case strings.HasSuffix(path, "/admin/members") && method == "GET":
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_members_list_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		mem, err := listMembers(ctx, org)
		if err != nil {
			return resp(500, map[string]string{"error": err.Error()}, origin)
		}
		// GOV-SEC-008: a team-scoped caller sees only its own team's roster, and
		// e-mail addresses are masked for any non-admin role (owner/admin see full).
		if teamScoped {
			filtered := make([]member, 0, len(mem))
			for _, m := range mem {
				if authCtx.CanAccessTeam(org, m.Team) {
					filtered = append(filtered, m)
				}
			}
			mem = filtered
		}
		if !canAdmin {
			for i := range mem {
				mem[i].Email = maskEmail(mem[i].Email)
			}
		}
		return resp(200, map[string]interface{}{
			"members": mem, "count": len(mem),
		}, origin)

	case strings.HasSuffix(path, "/admin/members") && method == "POST":
		// Simplified for single-org deployment: org is always from token, no dynamic org creation
		var b struct {
			Email    string   `json:"email"`
			Password string   `json:"password"`
			Name     string   `json:"name"`
			Role     string   `json:"role"`
			Team     string   `json:"team"`
			Apps     []string `json:"apps"`
		}
		if err := ReadAndValidateJSON(req.Body, &b); err != nil {
			return resp(400, map[string]interface{}{"error": err.Error()}, origin)
		}

		// Validate fields
		if errs := validateMemberUpdate(b); len(errs) > 0 {
			return resp(400, map[string]interface{}{
				"error":   "validation failed",
				"details": errs,
			}, origin)
		}
		// Org from the token for a regular user; a platform_admin must pass
		// ?org= explicitly (same pattern as every other admin/* handler —
		// GET/PUT /admin/members, /admin/teams, /admin/apps, etc). The previous
		// forceOrg("") here made platform_admin unable to invite ANY member,
		// since ForceOrg returns the literal empty param for platform_admin.
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_member_invite_attempt", fmt.Sprintf("org:%s,email:%s", org, b.Email), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		// Only owner/admin (or platform_admin) invite members
		if !isPlatform && role != "owner" && role != "admin" {
			return resp(403, map[string]string{"error": "only an owner or admin can invite members"}, origin)
		}
		b.Email = strings.TrimSpace(strings.ToLower(b.Email))
		b.Role = strings.TrimSpace(strings.ToLower(b.Role))
		b.Team = strings.TrimSpace(b.Team)
		b.Name = strings.TrimSpace(b.Name)
		if b.Email == "" || !strings.Contains(b.Email, "@") {
			return resp(400, map[string]string{"error": "invalid e-mail"}, origin)
		}
		if !memberRoles[b.Role] {
			return resp(400, map[string]string{"error": "invalid role (owner|admin|billing|dev)"}, origin)
		}
		// GOV-SEC-007: only an owner (or platform_admin) may mint another owner.
		// An admin can manage admin/billing/dev but must not escalate the hierarchy.
		if b.Role == "owner" && role != "owner" && !isPlatform {
			return resp(403, map[string]string{"error": "only an owner can grant the owner role"}, origin)
		}
		// Single-deployment: no plan-derived seat ceiling or team requirement.
		// A team is optional — the customer either uses teams or doesn't, and
		// b.Team simply stays "" when they don't.
		// GOV-SEC-009: a named team must already exist in the org registry.
		if !memberTeamOK(ctx, org, b.Team) {
			return resp(400, map[string]string{"error": "unknown_team", "message": "team does not exist in this org: " + b.Team}, origin)
		}
		if _, err := listMembers(ctx, org); err != nil {
			return resp(500, map[string]string{"error": err.Error()}, origin)
		}
		// Creation through the identity port. Only platform_admin creates with a
		// permanent password (no e-mail invite); everyone else gets an invite. The
		// role lock (password only for platform) stays HERE, in the shell; the
		// adapter only runs the chosen mechanism.
		pw := ""
		if isPlatform && b.Password != "" {
			pw = b.Password
		}
		// GOV-SEC-011: the create returns the new user's sub directly (from the
		// AdminCreateUser response). Writing the MEMBER# record keyed by that sub needs no
		// second AdminGetUser, so there is no window where the user exists in Cognito but a
		// throttled follow-up lookup fails and leaves no access record (a dev would then
		// sign in org-wide — see the pretoken fail-closed change).
		sub, err := identity.CreateUser(ctx, ports.User{Email: b.Email, Org: org, Role: b.Role, Name: b.Name}, pw)
		if err != nil {
			if errors.Is(err, ports.ErrUserExists) {
				return resp(409, map[string]string{"error": "e-mail already registered"}, origin)
			}
			return resp(500, map[string]string{"error": err.Error()}, origin)
		}
		// Belt-and-suspenders: if the create response carried no sub (older pool, or the
		// double in a test path), fall back to the e-mail→sub lookup.
		if sub == "" {
			s, ok := subFor(ctx, b.Email)
			if !ok {
				return resp(500, map[string]string{"error": "member created but could not resolve its identity id; retry"}, origin)
			}
			sub = s
		}
		writeMemberMeta(ctx, org, sub, b.Team, b.Apps)
		status := "invited"
		if isPlatform && b.Password != "" {
			status = "active"
		}
		writeAudit(ctx, org, claimEmail, role, govcore.AuditMemberInvite, b.Email, "role="+b.Role+" team="+b.Team)
		return resp(200, map[string]interface{}{"email": b.Email, "role": b.Role, "team": b.Team, "apps": b.Apps, "org": org, "status": status}, origin)

	case strings.HasSuffix(path, "/admin/members") && method == "PUT":
		// GOV-SEC-007: platform_admin must pass ?org= explicitly (forceOrg returns the
		// literal param for platform), same as every other admin/* route. forceOrg("")
		// used to lock operators out of member mutation entirely.
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}
		if !isPlatform && role != "owner" && role != "admin" {
			return resp(403, map[string]string{"error": "only an owner or admin can edit member access"}, origin)
		}
		var b struct {
			Email string   `json:"email"`
			Role  string   `json:"role"`
			Team  string   `json:"team"`
			Apps  []string `json:"apps"`
		}
		if err := ReadAndValidateJSON(req.Body, &b); err != nil {
			return resp(400, map[string]interface{}{"error": err.Error()}, origin)
		}

		// Validate fields (reusing the same validator with empty password/name)
		if errs := validateMemberUpdate(struct {
			Email    string   `json:"email"`
			Password string   `json:"password"`
			Name     string   `json:"name"`
			Role     string   `json:"role"`
			Team     string   `json:"team"`
			Apps     []string `json:"apps"`
		}{Email: b.Email, Role: b.Role, Team: b.Team, Apps: b.Apps}); len(errs) > 0 {
			return resp(400, map[string]interface{}{
				"error":   "validation failed",
				"details": errs,
			}, origin)
		}
		b.Email = strings.TrimSpace(strings.ToLower(b.Email))
		b.Role = strings.TrimSpace(strings.ToLower(b.Role))
		b.Team = strings.TrimSpace(b.Team)
		if b.Email == "" {
			return resp(400, map[string]string{"error": "email is required"}, origin)
		}
		if !memberRoles[b.Role] {
			return resp(400, map[string]string{"error": "invalid role (owner|admin|billing|dev)"}, origin)
		}
		// GOV-SEC-009: a named team must already exist in the org registry.
		if !memberTeamOK(ctx, org, b.Team) {
			return resp(400, map[string]string{"error": "unknown_team", "message": "team does not exist in this org: " + b.Team}, origin)
		}
		// GOV-SEC-007: promoting someone to owner requires owner (or platform) rights.
		if b.Role == "owner" && role != "owner" && !isPlatform {
			return resp(403, map[string]string{"error": "only an owner can grant the owner role"}, origin)
		}
		// Ownership check: the member must belong to the SAME org.
		uorg, _ := memberOrg(ctx, b.Email)
		if uorg != org && !isPlatform {
			return resp(403, map[string]string{"error": "that member does not belong to your organization"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can modify this member
		if !authCtx.CanModifyMember(org, b.Email) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_member_modification_attempt", fmt.Sprintf("org:%s,email:%s", org, b.Email), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}
		// Safeguard: never demote the LAST owner (the org would be left ownerless).
		if tr, owners := ownerStats(ctx, org, b.Email); tr == "owner" && b.Role != "owner" && owners <= 1 {
			return resp(409, map[string]string{"error": "cannot demote the only owner — promote another owner first"}, origin)
		}
		// Single-deployment: team is optional, never plan-mandated.
		// Update the role in Cognito (authorization claim) through the port.
		if err := identity.UpdateAttrs(ctx, b.Email, map[string]string{"custom:role": b.Role}); err != nil {
			return resp(500, map[string]string{"error": "failed to update the role: " + err.Error()}, origin)
		}
		// GOV-SEC-003: the access record is keyed by the member's immutable sub.
		sub, ok := subFor(ctx, b.Email)
		if !ok {
			return resp(500, map[string]string{"error": "could not resolve the member identity id; retry"}, origin)
		}
		writeMemberMeta(ctx, org, sub, b.Team, b.Apps)
		writeAudit(ctx, org, claimEmail, role, govcore.AuditMemberUpdate, b.Email, "role="+b.Role+" team="+b.Team)
		return resp(200, map[string]interface{}{"email": b.Email, "role": b.Role, "team": b.Team, "apps": b.Apps, "status": "updated"}, origin)

	case strings.HasSuffix(path, "/admin/members") && method == "DELETE":
		// GOV-SEC-007: platform_admin must pass ?org= explicitly (forceOrg returns the
		// literal param for platform), same as every other admin/* route. forceOrg("")
		// used to lock operators out of member mutation entirely.
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}
		if !isPlatform && role != "owner" && role != "admin" {
			return resp(403, map[string]string{"error": "only an owner or admin can remove members"}, origin)
		}
		email := strings.TrimSpace(strings.ToLower(req.QueryStringParameters["email"]))
		if email == "" {
			return resp(400, map[string]string{"error": "email is required"}, origin)
		}
		// Ownership check: only removes someone from the SAME org (destructive).
		uorg, _ := memberOrg(ctx, email)
		if uorg != org && !isPlatform {
			return resp(403, map[string]string{"error": "that member does not belong to your organization"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can modify this member
		if !authCtx.CanModifyMember(org, email) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_member_deletion_attempt", fmt.Sprintf("org:%s,email:%s", org, email), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}
		// Safeguard: never remove the LAST owner.
		if tr, owners := ownerStats(ctx, org, email); tr == "owner" && owners <= 1 {
			return resp(409, map[string]string{"error": "cannot remove the only owner — transfer ownership first"}, origin)
		}
		// GOV-SEC-003: resolve the member's sub BEFORE deleting the Cognito user —
		// once deleted, AdminGetUser can no longer return the sub, so the access
		// record would be orphaned.
		delSub, delSubOK := subFor(ctx, email)
		if err := identity.DeleteUser(ctx, email); err != nil {
			return resp(500, map[string]string{"error": err.Error()}, origin)
		}
		// Clean up the member's access record (best-effort), keyed by sub.
		if delSubOK {
			ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &configTable,
				Key: map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: memberMetaKey(org, delSub)}}})
		}
		writeAudit(ctx, org, claimEmail, role, govcore.AuditMemberRemove, email, "")
		return resp(200, map[string]interface{}{"email": email, "status": "removed"}, origin)

	// ---- Member password: reset by e-mail (an admin NEVER sees or sets the password) ----
	case strings.HasSuffix(path, "/admin/members/password") && method == "POST":
		// GOV-SEC-007: platform_admin must pass ?org= explicitly (forceOrg returns the
		// literal param for platform), same as every other admin/* route. forceOrg("")
		// used to lock operators out of member mutation entirely.
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}
		if !isPlatform && role != "owner" && role != "admin" {
			return resp(403, map[string]string{"error": "only an owner or admin can reset a member's password"}, origin)
		}
		var b struct {
			Email string `json:"email"`
		}
		if err := ReadAndValidateJSON(req.Body, &b); err != nil {
			return resp(400, map[string]interface{}{"error": err.Error()}, origin)
		}
		b.Email = strings.TrimSpace(strings.ToLower(b.Email))
		if b.Email == "" {
			return resp(400, map[string]string{"error": "email is required"}, origin)
		}
		if uorg, _ := memberOrg(ctx, b.Email); uorg != org && !isPlatform {
			return resp(403, map[string]string{"error": "that member does not belong to your organization"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can modify this member
		if !authCtx.CanModifyMember(org, b.Email) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_password_reset_attempt", fmt.Sprintf("org:%s,email:%s", org, b.Email), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		if err := identity.ResetPassword(ctx, b.Email); err != nil {
			return resp(500, map[string]string{"error": err.Error()}, origin)
		}
		writeAudit(ctx, org, claimEmail, role, govcore.AuditPasswordReset, b.Email, "reset por e-mail")
		return resp(200, map[string]interface{}{"email": b.Email, "status": "reset_email_sent"}, origin)

	// ---- Resend invite (new temporary password by e-mail) ----
	case strings.HasSuffix(path, "/admin/members/resend") && method == "POST":
		// GOV-SEC-007: platform_admin must pass ?org= explicitly (forceOrg returns the
		// literal param for platform), same as every other admin/* route. forceOrg("")
		// used to lock operators out of member mutation entirely.
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}
		if !isPlatform && role != "owner" && role != "admin" {
			return resp(403, map[string]string{"error": "only an owner or admin can resend invites"}, origin)
		}
		var b struct {
			Email string `json:"email"`
		}
		if err := ReadAndValidateJSON(req.Body, &b); err != nil {
			return resp(400, map[string]interface{}{"error": err.Error()}, origin)
		}
		b.Email = strings.TrimSpace(strings.ToLower(b.Email))
		if b.Email == "" {
			return resp(400, map[string]string{"error": "email is required"}, origin)
		}
		if uorg, _ := memberOrg(ctx, b.Email); uorg != org && !isPlatform {
			return resp(403, map[string]string{"error": "that member does not belong to your organization"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can modify this member
		if !authCtx.CanModifyMember(org, b.Email) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_invite_resend_attempt", fmt.Sprintf("org:%s,email:%s", org, b.Email), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		if err := identity.ResendInvite(ctx, b.Email); err != nil {
			return resp(500, map[string]string{"error": err.Error()}, origin)
		}
		writeAudit(ctx, org, claimEmail, role, govcore.AuditInviteResend, b.Email, "convite reenviado")
		return resp(200, map[string]interface{}{"email": b.Email, "status": "invite_resent"}, origin)

	// ---- Enable/disable account (reversible block, without deleting) ----
	case strings.HasSuffix(path, "/admin/members/enable") && method == "POST":
		// GOV-SEC-007: platform_admin must pass ?org= explicitly (forceOrg returns the
		// literal param for platform), same as every other admin/* route. forceOrg("")
		// used to lock operators out of member mutation entirely.
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}
		if !isPlatform && role != "owner" && role != "admin" {
			return resp(403, map[string]string{"error": "only an owner or admin can enable/disable members"}, origin)
		}
		var b struct {
			Email   string `json:"email"`
			Enabled bool   `json:"enabled"`
		}
		if err := ReadAndValidateJSON(req.Body, &b); err != nil {
			return resp(400, map[string]interface{}{"error": err.Error()}, origin)
		}
		b.Email = strings.TrimSpace(strings.ToLower(b.Email))
		if b.Email == "" {
			return resp(400, map[string]string{"error": "email is required"}, origin)
		}
		if uorg, _ := memberOrg(ctx, b.Email); uorg != org && !isPlatform {
			return resp(403, map[string]string{"error": "that member does not belong to your organization"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can modify this member
		if !authCtx.CanModifyMember(org, b.Email) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_member_enable_disable_attempt", fmt.Sprintf("org:%s,email:%s,enabled:%t", org, b.Email, b.Enabled), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}
		// Safeguard: never disable the LAST owner (the org would lose its active owner).
		if !b.Enabled {
			if tr, owners := ownerStats(ctx, org, b.Email); tr == "owner" && owners <= 1 {
				return resp(409, map[string]string{"error": "cannot disable the only owner — promote another owner first"}, origin)
			}
		}
		if err := identity.SetEnabled(ctx, b.Email, b.Enabled); err != nil {
			return resp(500, map[string]string{"error": err.Error()}, origin)
		}
		action := govcore.AuditMemberEnable
		if !b.Enabled {
			action = govcore.AuditMemberDisable
		}
		writeAudit(ctx, org, claimEmail, role, action, b.Email, "")
		return resp(200, map[string]interface{}{"email": b.Email, "enabled": b.Enabled, "status": "updated"}, origin)

	// ================= Teams & Apps (first-class objects) =================
	// Metadata (name/status/creation) in the TEAMS#<org> record. The team's policy
	// (budget/allowed_models) stays in PUT /admin/config?team=<id>.
	// Writing requires owner/admin (canAdmin); a team-scoped user is locked to its
	// own team.

	case strings.HasSuffix(path, "/admin/teams") && method == "GET":
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_teams_access_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		t := readOrgTree(ctx, org)

		// ?summary=1: the console's Members tab (Team dropdown) only needs the
		// active team/app IDs to populate a <select> — not the full metadata
		// (display_name/status/created_at/origin) for every team/app. An org
		// with hundreds of persisted teams/apps (real usage, or a stress-test
		// leftover) made the full payload big enough to look hung while the
		// browser parsed and deduped it. Same auth/scoping as the full
		// response below, just a smaller response shape. Mirrors the
		// ?summary=1 already added to GET /admin/keys (keyadmin domain).
		if req.QueryStringParameters["summary"] == "1" {
			teamIDs := []string{}
			for id, m := range t.Teams {
				if teamScoped && id != claimTeam {
					continue
				}
				if m.Status == govcore.StatusArchived {
					continue
				}
				teamIDs = append(teamIDs, id)
			}
			appIDs := []string{}
			for id, m := range t.Apps {
				if teamScoped && m.Team != claimTeam {
					continue
				}
				if m.Status == govcore.StatusArchived {
					continue
				}
				appIDs = append(appIDs, id)
			}
			sort.Strings(teamIDs)
			sort.Strings(appIDs)
			return resp(200, map[string]interface{}{"teams": teamIDs, "apps": appIDs}, origin)
		}

		teams := []map[string]interface{}{}
		for id, m := range t.Teams {
			if teamScoped && id != claimTeam {
				continue // a dev locked to a team only sees their own
			}
			teams = append(teams, map[string]interface{}{
				"id": id, "display_name": m.DisplayName, "status": m.Status,
				"created_at": m.CreatedAt, "origin": "persisted",
			})
		}
		apps := []map[string]interface{}{}
		for id, m := range t.Apps {
			if teamScoped && m.Team != claimTeam {
				continue
			}
			apps = append(apps, map[string]interface{}{
				"id": id, "team": m.Team, "display_name": m.DisplayName,
				"status": m.Status, "created_at": m.CreatedAt, "origin": "persisted",
			})
		}
		return resp(200, map[string]interface{}{"teams": teams, "apps": apps}, origin)

	// GET /admin/apps?org=&team= — lists apps (optionally filtered by team).
	// Convenience shortcut: GET /admin/teams already returns apps, but this
	// endpoint allows filtering only a team's apps without loading the teams.
	case strings.HasSuffix(path, "/admin/apps") && method == "GET":
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_apps_access_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		filterTeam := req.QueryStringParameters["team"]
		t := readOrgTree(ctx, org)
		apps := []map[string]interface{}{}
		for id, m := range t.Apps {
			if teamScoped && m.Team != claimTeam {
				continue
			}
			if filterTeam != "" && m.Team != filterTeam {
				continue
			}
			apps = append(apps, map[string]interface{}{
				"id": id, "team": m.Team, "display_name": m.DisplayName,
				"status": m.Status, "created_at": m.CreatedAt, "origin": "persisted",
			})
		}
		return resp(200, map[string]interface{}{"apps": apps, "count": len(apps)}, origin)

	case strings.HasSuffix(path, "/admin/teams") && method == "POST":
		if !canAdmin {
			return resp(403, map[string]string{"error": "only owner/admin can create teams"}, origin)
		}
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_team_creation_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		var b struct {
			DisplayName string `json:"display_name"`
		}
		if err := ReadAndValidateJSON(req.Body, &b); err != nil {
			return resp(400, map[string]interface{}{"error": err.Error()}, origin)
		}

		// Validate fields
		if errs := validateTeamMeta(b); len(errs) > 0 {
			return resp(400, map[string]interface{}{
				"error":   "validation failed",
				"details": errs,
			}, origin)
		}

		if !govcore.ValidName(b.DisplayName) {
			return resp(400, map[string]string{"error": "invalid name (1 to 64 characters)"}, origin)
		}
		// updateOrgTree retries the whole read-modify-write under contention (see
		// its doc comment) — id is recomputed from whatever tree it last read, so
		// a concurrent create never clobbers or gets clobbered by another one.
		var id string
		_, err := updateOrgTree(ctx, org, func(t govcore.OrgTree) (govcore.OrgTree, error) {
			id = govcore.SlugID(b.DisplayName, govcore.TakenTeamIDs(t))
			return govcore.AddTeam(t, id, b.DisplayName, time.Now().UTC().Format(time.RFC3339))
		})
		if err != nil {
			return resp(teamErrStatus(err), map[string]string{"error": err.Error()}, origin)
		}
		writeAudit(ctx, org, claimEmail, role, "team.create", id, b.DisplayName)
		return resp(200, map[string]interface{}{"id": id, "display_name": b.DisplayName, "status": govcore.StatusActive}, origin)

	case strings.HasSuffix(path, "/admin/teams") && method == "PUT":
		if !canAdmin {
			return resp(403, map[string]string{"error": "only owner/admin can rename teams"}, origin)
		}
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_team_modification_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		var b struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		}
		if err := ReadAndValidateJSON(req.Body, &b); err != nil {
			return resp(400, map[string]interface{}{"error": err.Error()}, origin)
		}

		// Validate display_name field
		if errs := validateTeamMeta(struct {
			DisplayName string `json:"display_name"`
		}{DisplayName: b.DisplayName}); len(errs) > 0 {
			return resp(400, map[string]interface{}{
				"error":   "validation failed",
				"details": errs,
			}, origin)
		}

		// Read org tree to verify team exists and belongs to org
		t := readOrgTree(ctx, org)
		if _, exists := t.Teams[b.ID]; !exists {
			return resp(404, map[string]string{"error": "team not found"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access this specific team
		if !authCtx.CanAccessTeam(org, b.ID) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_team_modification_attempt", fmt.Sprintf("org:%s,team:%s", org, b.ID), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		if teamScoped && b.ID != claimTeam {
			return resp(403, map[string]string{"error": "outside your team"}, origin)
		}
		// t above was only used for the existence/authorization pre-checks; the
		// actual mutation re-reads inside updateOrgTree (retried under
		// contention — see its doc comment) so it never acts on a stale tree.
		_, err := updateOrgTree(ctx, org, func(cur govcore.OrgTree) (govcore.OrgTree, error) {
			return govcore.RenameTeam(cur, b.ID, b.DisplayName)
		})
		if err != nil {
			return resp(teamErrStatus(err), map[string]string{"error": err.Error()}, origin)
		}
		writeAudit(ctx, org, claimEmail, role, "team.rename", b.ID, b.DisplayName)
		return resp(200, map[string]interface{}{"id": b.ID, "display_name": b.DisplayName, "status": "updated"}, origin)

	case strings.HasSuffix(path, "/admin/teams") && method == "DELETE":
		if !canAdmin {
			return resp(403, map[string]string{"error": "only owner/admin can archive/remove teams"}, origin)
		}
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_team_deletion_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		id := req.QueryStringParameters["id"]
		mode := req.QueryStringParameters["mode"]

		// Read org tree to verify team exists and belongs to org
		t := readOrgTree(ctx, org)
		if _, exists := t.Teams[id]; !exists {
			return resp(404, map[string]string{"error": "team not found"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access this specific team
		if !authCtx.CanAccessTeam(org, id) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_team_deletion_attempt", fmt.Sprintf("org:%s,team:%s", org, id), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		if teamScoped && id != claimTeam {
			return resp(403, map[string]string{"error": "outside your team"}, origin)
		}
		_, err := updateOrgTree(ctx, org, func(cur govcore.OrgTree) (govcore.OrgTree, error) {
			if mode == "remove" {
				return govcore.RemoveTeam(cur, id)
			}
			return govcore.SetTeamStatus(cur, id, govcore.StatusArchived)
		})
		if err != nil {
			return resp(teamErrStatus(err), map[string]string{"error": err.Error()}, origin)
		}
		writeAudit(ctx, org, claimEmail, role, "team."+orDefault(mode, "archive"), id, "")
		return resp(200, map[string]interface{}{"id": id, "mode": orDefault(mode, "archive"), "status": "ok"}, origin)

	case strings.HasSuffix(path, "/admin/apps") && method == "POST":
		if !canAdmin {
			return resp(403, map[string]string{"error": "only owner/admin can create apps"}, origin)
		}
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_app_creation_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		var b struct {
			Team        string `json:"team"`
			DisplayName string `json:"display_name"`
		}
		if err := ReadAndValidateJSON(req.Body, &b); err != nil {
			return resp(400, map[string]interface{}{"error": err.Error()}, origin)
		}

		// Validate display_name field
		if errs := validateTeamMeta(struct {
			DisplayName string `json:"display_name"`
		}{DisplayName: b.DisplayName}); len(errs) > 0 {
			return resp(400, map[string]interface{}{
				"error":   "validation failed",
				"details": errs,
			}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested team
		if !authCtx.CanAccessTeam(org, b.Team) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_app_creation_attempt", fmt.Sprintf("org:%s,team:%s", org, b.Team), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		if teamScoped && b.Team != claimTeam {
			return resp(403, map[string]string{"error": "outside your team"}, origin)
		}
		if !govcore.ValidName(b.DisplayName) {
			return resp(400, map[string]string{"error": "invalid name (1 to 64 characters)"}, origin)
		}
		var id string
		_, err := updateOrgTree(ctx, org, func(t govcore.OrgTree) (govcore.OrgTree, error) {
			id = govcore.SlugID(b.DisplayName, govcore.TakenAppIDs(t))
			return govcore.AddApp(t, id, b.Team, b.DisplayName, time.Now().UTC().Format(time.RFC3339))
		})
		if err != nil {
			return resp(teamErrStatus(err), map[string]string{"error": err.Error()}, origin)
		}
		writeAudit(ctx, org, claimEmail, role, "app.create", id, b.Team)
		return resp(200, map[string]interface{}{"id": id, "team": b.Team, "display_name": b.DisplayName, "status": govcore.StatusActive}, origin)

	case strings.HasSuffix(path, "/admin/apps") && method == "PUT":
		if !canAdmin {
			return resp(403, map[string]string{"error": "only owner/admin can rename apps"}, origin)
		}
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_app_modification_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		var b struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		}
		if err := ReadAndValidateJSON(req.Body, &b); err != nil {
			return resp(400, map[string]interface{}{"error": err.Error()}, origin)
		}

		// Validate display_name field
		if errs := validateTeamMeta(struct {
			DisplayName string `json:"display_name"`
		}{DisplayName: b.DisplayName}); len(errs) > 0 {
			return resp(400, map[string]interface{}{
				"error":   "validation failed",
				"details": errs,
			}, origin)
		}

		// Read org tree to verify app exists and belongs to org
		t := readOrgTree(ctx, org)
		app, exists := t.Apps[b.ID]
		if !exists {
			return resp(404, map[string]string{"error": "app not found"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the app's team
		if !authCtx.CanAccessTeam(org, app.Team) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_app_modification_attempt", fmt.Sprintf("org:%s,app:%s,team:%s", org, b.ID, app.Team), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		if teamScoped {
			if a, ok := t.Apps[b.ID]; !ok || a.Team != claimTeam {
				return resp(403, map[string]string{"error": "outside your team"}, origin)
			}
		}
		_, err := updateOrgTree(ctx, org, func(cur govcore.OrgTree) (govcore.OrgTree, error) {
			return govcore.RenameApp(cur, b.ID, b.DisplayName)
		})
		if err != nil {
			return resp(teamErrStatus(err), map[string]string{"error": err.Error()}, origin)
		}
		writeAudit(ctx, org, claimEmail, role, "app.rename", b.ID, b.DisplayName)
		return resp(200, map[string]interface{}{"id": b.ID, "display_name": b.DisplayName, "status": "updated"}, origin)

	case strings.HasSuffix(path, "/admin/apps") && method == "DELETE":
		if !canAdmin {
			return resp(403, map[string]string{"error": "only owner/admin can archive/remove apps"}, origin)
		}
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_app_deletion_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		id := req.QueryStringParameters["id"]
		mode := req.QueryStringParameters["mode"]

		// Read org tree to verify app exists and belongs to org
		t := readOrgTree(ctx, org)
		app, exists := t.Apps[id]
		if !exists {
			return resp(404, map[string]string{"error": "app not found"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the app's team
		if !authCtx.CanAccessTeam(org, app.Team) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_app_deletion_attempt", fmt.Sprintf("org:%s,app:%s,team:%s", org, id, app.Team), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		if teamScoped {
			if a, ok := t.Apps[id]; !ok || a.Team != claimTeam {
				return resp(403, map[string]string{"error": "outside your team"}, origin)
			}
		}
		_, err := updateOrgTree(ctx, org, func(cur govcore.OrgTree) (govcore.OrgTree, error) {
			if mode == "remove" {
				return govcore.RemoveApp(cur, id)
			}
			return govcore.SetAppStatus(cur, id, govcore.StatusArchived)
		})
		if err != nil {
			return resp(teamErrStatus(err), map[string]string{"error": err.Error()}, origin)
		}
		writeAudit(ctx, org, claimEmail, role, "app."+orDefault(mode, "archive"), id, "")
		return resp(200, map[string]interface{}{"id": id, "mode": orDefault(mode, "archive"), "status": "ok"}, origin)

	// ---- Control plane auditing (trail of administrative actions) ----
	case strings.HasSuffix(path, "/admin/audit") && method == "GET":
		org, okScope := forceOrg(req.QueryStringParameters["org"])
		if !okScope || org == "" {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_audit_access_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		if !isPlatform && role != "owner" && role != "admin" {
			return resp(403, map[string]string{"error": "only an owner or admin can view the audit trail"}, origin)
		}
		rows, err := listAudit(ctx, org, 100)
		if err != nil {
			return resp(500, map[string]string{"error": err.Error()}, origin)
		}
		return resp(200, map[string]interface{}{"entries": rows, "count": len(rows)}, origin)

	// ---- Bedrock Models (lists the customer account's models via AssumeRole) ----
	case strings.HasSuffix(path, "/admin/bedrock/models") && method == "GET":
		// GOV-SEC-002: only owner/admin, and a caller-supplied role_arn must be an
		// AIPlatGatewayAccess role the org ALREADY declares on one of its routes — never
		// an arbitrary ARN. Before this, any authenticated member (dev/billing included)
		// could make the platform sts:AssumeRole into any account and read the raw STS
		// error (which names the execution role).
		if !canAdmin {
			return resp(403, map[string]string{"error": "your role cannot list Bedrock models (owner/admin only)"}, origin)
		}
		q := req.QueryStringParameters
		roleARN := q["role_arn"]
		externalID := q["external_id"]
		region := q["region"]
		if region == "" {
			region = "us-east-1"
		}
		if roleARN != "" {
			secOrg, okScope := forceOrg(q["org"])
			if !okScope || secOrg == "" {
				return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
			}
			if !gatewayRoleARNRe.MatchString(roleARN) || !orgDeclaresRoleARN(ctx, secOrg, roleARN) {
				writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role, "unauthorized_bedrock_models_role", roleARN, "")
				return resp(400, map[string]string{"error": "role_arn must be an AIPlatGatewayAccess role already configured on one of this org's routes"}, origin)
			}
		}

		models, err := listBedrockModels(ctx, roleARN, externalID, region)
		if err != nil {
			// Generic: the raw STS/Bedrock error names the execution role ARN and account.
			return resp(502, map[string]string{"error": "could not list models for that role"}, origin)
		}

		return resp(200, map[string]interface{}{
			"models": models,
			"count":  len(models),
			"region": region,
			"byo":    roleARN != "",
		}, origin)

	// ---- BYO Bedrock model listing, REQUEST-SCOPED (GOV-SEC-010) ----
	// The console BYO wizard needs to list a new account's models BEFORE any route carries
	// its role_arn. The GET above requires the ARN to be already declared on a route
	// (GOV-SEC-002), which made the wizard dead-lock — round 3 "fixed" that by writing a
	// placeholder routing entry (a callable data-plane route holding the customer's role_arn
	// with no model id). This POST replaces that: it lists models for a caller-supplied ARN
	// in the request body, WITHOUT writing any config. The AssumeRole-oracle protections are
	// kept: owner/admin only, the ARN must have the AIPlatGatewayAccess* FORM, and the error
	// is generic (never the raw STS/Bedrock error that names the execution role/account).
	case strings.HasSuffix(path, "/admin/bedrock/models") && method == "POST":
		if !canAdmin {
			return resp(403, map[string]string{"error": "your role cannot list Bedrock models (owner/admin only)"}, origin)
		}
		var b struct {
			RoleARN    string `json:"role_arn"`
			ExternalID string `json:"external_id"`
			Region     string `json:"region"`
		}
		if err := ReadAndValidateJSON(req.Body, &b); err != nil {
			return resp(400, map[string]interface{}{"error": err.Error()}, origin)
		}
		b.RoleARN = strings.TrimSpace(b.RoleARN)
		b.Region = strings.TrimSpace(b.Region)
		if b.Region == "" {
			b.Region = "us-east-1"
		}
		if b.RoleARN == "" {
			return resp(400, map[string]string{"error": "role_arn is required"}, origin)
		}
		// FORM check only — NOT orgDeclaresRoleARN, so a brand-new account can be listed
		// before it has a route. The form + owner/admin gate + generic error keep the
		// oracle closed (a caller learns only "could list" / "could not", never the raw
		// STS error). No config is written here.
		if !gatewayRoleARNRe.MatchString(b.RoleARN) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role, "unauthorized_bedrock_models_role", b.RoleARN, "")
			return resp(400, map[string]string{"error": "role_arn must be an AIPlatGatewayAccess role (arn:aws:iam::<account>:role/AIPlatGatewayAccess*)"}, origin)
		}
		models, err := listBedrockModels(ctx, b.RoleARN, b.ExternalID, b.Region)
		if err != nil {
			return resp(502, map[string]string{"error": "could not list models for that role"}, origin)
		}
		return resp(200, map[string]interface{}{"models": models, "count": len(models), "region": b.Region, "byo": true}, origin)

	// ---- Provider Models (lists an external/self-hosted provider's models) ----
	// Uses the org's credential kept in the vault (aiplat/org/<org>/<provider>) —
	// the key never leaves for the client. Per-org scope from the token.
	case strings.HasSuffix(path, "/admin/provider/models") && method == "GET":
		// Role: this call puts the org's provider credential on the wire to base_url, so
		// it is a credential operation, not a read — same gate as POST /admin/secrets.
		if !canAdmin {
			return resp(403, map[string]string{"error": "your role cannot list provider models (owner/admin only)"}, origin)
		}
		q := req.QueryStringParameters
		org, okScope := forceOrg(q["org"])
		if !okScope {
			return resp(403, map[string]string{"error": "org could not be determined from the token"}, origin)
		}

		// CHECK AUTHORIZATION: Verify user can access the requested org
		if !authCtx.CanAccessOrg(org) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"unauthorized_provider_models_access_attempt", fmt.Sprintf("org:%s", org), "")
			return resp(403, map[string]string{"error": "access denied"}, origin)
		}

		provider := strings.ToLower(strings.TrimSpace(q["provider"]))
		baseURL := strings.TrimSpace(q["base_url"])
		if provider == "" || baseURL == "" {
			return resp(400, map[string]string{"error": "provider and base_url are required"}, origin)
		}
		// The credential may only travel to a destination the gateway itself already calls:
		// a base_url declared on one of the org's routes. Anything else is refused before the
		// secret is read.
		if !orgDeclaresBaseURL(ctx, org, baseURL) {
			writeAudit(ctx, authCtx.Org, authCtx.Email, authCtx.Role,
				"provider_models_undeclared_base_url", fmt.Sprintf("org:%s,base_url:%s", org, baseURL), "")
			return resp(400, map[string]string{"error": "base_url must match a base_url already saved on one of this org's routes"}, origin)
		}
		key, err := secrets.Get(ctx, "aiplat/org/"+org+"/"+provider)
		if err != nil || key == "" {
			return resp(400, map[string]string{"error": "save the provider key before listing models"}, origin)
		}
		models, err := listProviderModels(ctx, strings.TrimSpace(q["adapter"]), baseURL, key)
		if err != nil {
			return resp(502, map[string]string{"error": "failed to list at the provider: " + err.Error()}, origin)
		}
		return resp(200, map[string]interface{}{"models": models, "count": len(models)}, origin)
	}

	return resp(404, map[string]string{"error": "not found"}, origin)
}

func main() {
	cfg, _ := awscfg.LoadDefaultConfig(context.TODO())
	baseCfg = cfg
	ddb = dynamodb.NewFromConfig(cfg)
	sm = secretsmanager.NewFromConfig(cfg)
	stsClient = sts.NewFromConfig(cfg)
	// Wiring of the outbound ports: the concrete adapters come in only here.
	identity = cognitosigv4.New(cfg, userPool, httpc)
	secrets = smsecrets.New(sm)
	rateLimiter = &RateLimiter{
		table: rateLimitTable,
		ddb:   ddb,
	}
	lambda.Start(handle)
}
