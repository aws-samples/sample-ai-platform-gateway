// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Emission of control plane audit events (Governance domain).
//
// This file is LOCAL to Governance on purpose. The action vocabulary is
// duplicated with respect to the Audit domain and coherence between the two is
// guaranteed by a shared FIXTURE
// (testdata/contracts/audit-trail/action-catalog.json), never by import — a
// domain does not import another domain, and that is verified by a test.
//
// Two design rules that cannot be loosened:
//
//  1. Emission NEVER fails the customer's action. Saving a configuration cannot
//     depend on the audit service being up.
//  2. The structured log comes BEFORE publishing. If PutEvents fails, the line is
//     already in CloudWatch and the trail is reconstructible through Logs
//     Insights. A trail with a silent gap is worse than no trail at all, because
//     it gives false confidence.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// Auditable actions emitted by this domain. Values are FROZEN: an audit record is
// immutable, so renaming would leave the history with two names for the same
// thing. The seven member actions mirror govcore.Audit*.
const (
	audConfigUpdate = "config_update"
	audCreditUpdate = "credit_update"
	audCreditDelete = "credit_delete"

	// Bundle and swap policy get their own actions instead of disappearing inside
	// config_update, for the same reason as models: they are the attempt order and
	// the quality promise of a critical flow.
	audBundleAdd        = "bundle_add"
	audBundleUpdate     = "bundle_update"
	audBundleRemove     = "bundle_remove"
	audSwapPolicyUpdate = "swap_policy_update"

	audModelAdd          = "model_add"
	audModelUpdate       = "model_update"
	audModelRemove       = "model_remove"
	audProviderSecretSet = "provider_secret_set"

	audMemberInvite  = "member_invite"
	audMemberUpdate  = "member_update"
	audMemberRemove  = "member_remove"
	audMemberEnable  = "member_enable"
	audMemberDisable = "member_disable"
	audPasswordReset = "password_reset"
	audInviteResend  = "invite_resend"

	audTeamCreate  = "team_create"
	audTeamRename  = "team_rename"
	audTeamArchive = "team_archive"
	audTeamRemove  = "team_remove"
	audAppCreate   = "app_create"
	audAppRename   = "app_rename"
	audAppArchive  = "app_archive"
	audAppRemove   = "app_remove"

	audPlanChange = "plan_change"
	audOrgCreate  = "org_create"
)

// auditCategory maps action → category (the Console sub-tabs). It must match the
// fixture; there is a contract test for that.
var auditCategory = map[string]string{
	audConfigUpdate: "config",
	audCreditUpdate: "config",
	audCreditDelete: "config",

	// Bundle and swap policy belong to the `models` category: whoever audits "what
	// changed in the routing" looks in the same sub-tab where the models are, not
	// in config.
	audBundleAdd:        "models",
	audBundleUpdate:     "models",
	audBundleRemove:     "models",
	audSwapPolicyUpdate: "models",

	audModelAdd:          "models",
	audModelUpdate:       "models",
	audModelRemove:       "models",
	audProviderSecretSet: "models",

	audMemberInvite:  "members",
	audMemberUpdate:  "members",
	audMemberRemove:  "members",
	audMemberEnable:  "members",
	audMemberDisable: "members",
	audPasswordReset: "members",
	audInviteResend:  "members",
	audTeamCreate:    "members",
	audTeamRename:    "members",
	audTeamArchive:   "members",
	audTeamRemove:    "members",
	audAppCreate:     "members",
	audAppRename:     "members",
	audAppArchive:    "members",
	audAppRemove:     "members",

	audPlanChange: "account",
	audOrgCreate:  "account",
}

// auditChange is a field change. Mirrors auditcore.Change in the contract.
type auditChange struct {
	Path     string `json:"path"`
	Before   any    `json:"before,omitempty"`
	After    any    `json:"after,omitempty"`
	Redacted bool   `json:"redacted,omitempty"`
}

type auditActor struct {
	Email string `json:"email"`
	Sub   string `json:"sub,omitempty"`
	Role  string `json:"role"`
	Type  string `json:"type"`
}

type auditEvent struct {
	ContractVersion string        `json:"contract_version"`
	EventID         string        `json:"event_id"`
	Org             string        `json:"org"`
	Actor           auditActor    `json:"actor"`
	Action          string        `json:"action"`
	Category        string        `json:"category"`
	Scope           string        `json:"scope,omitempty"`
	Target          string        `json:"target,omitempty"`
	Detail          string        `json:"detail,omitempty"`
	Changes         []auditChange `json:"changes,omitempty"`
	ChangeCount     int           `json:"change_count"`
	Truncated       bool          `json:"truncated,omitempty"`
	SourceIP        string        `json:"source_ip,omitempty"`
	UserAgent       string        `json:"user_agent,omitempty"`
	TS              string        `json:"ts"`
}

const (
	auditContractVersion = "1"
	auditMaxChanges      = 100
	auditRedactedMarker  = "«redacted»"
)

// auditBus is the name of the Audit domain's EventBridge bus, injected through
// the environment (12-Factor). Empty turns emission off without breaking
// anything — that is what allows deploying this domain before audit exists.
var auditBus = os.Getenv("AUDIT_BUS")

// newAuditActor builds the actor. The e-mail is normalized to lowercase (the same
// normalization as govcore.NewAuditEntry) and the type is derived from the role.
func newAuditActor(email, sub, role string) auditActor {
	typ := "customer"
	if role == "platform_admin" {
		typ = "platform_operator"
	}
	return auditActor{
		Email: strings.ToLower(strings.TrimSpace(email)),
		Sub:   sub, Role: role, Type: typ,
	}
}

// auditSensitiveLeaves: field names whose VALUE can never be persisted.
// The comparison uses the LAST segment of the path — the difference matters:
//
//	"routing.x.api_key"        → the credential value           → REDACT
//	"routing.x.api_key_secret" → the NAME of the referenced secret → do NOT redact
//
// The secret's name is exactly what one wants to audit ("switched to credential
// X"); redacting it would make the trail useless in the real case.
var auditSensitiveLeaves = map[string]bool{
	"api_key": true, "apikey": true, "password": true, "passwd": true,
	"secret": true, "secret_value": true, "token": true, "access_token": true,
	"credential": true, "credentials": true, "private_key": true,
}

func auditIsSensitive(path string) bool {
	leaf := path
	if i := strings.LastIndex(path, "."); i >= 0 {
		leaf = path[i+1:]
	}
	return auditSensitiveLeaves[strings.ToLower(leaf)]
}

// auditDiff flattens both objects into dotted paths and returns one entry per
// changed field, in deterministic order. A list is compared as a single value (an
// array index is an unstable path).
func auditDiff(before, after map[string]interface{}) []auditChange {
	fb, fa := map[string]interface{}{}, map[string]interface{}{}
	auditFlatten("", before, fb)
	auditFlatten("", after, fa)

	seen := map[string]bool{}
	var paths []string
	for p := range fb {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for p := range fa {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	out := make([]auditChange, 0, len(paths))
	for _, p := range paths {
		b, hadB := fb[p]
		a, hadA := fa[p]
		if hadB && hadA && auditSame(b, a) {
			continue
		}
		ch := auditChange{Path: p}
		if hadB {
			ch.Before = b
		}
		if hadA {
			ch.After = a
		}
		if auditIsSensitive(p) {
			if ch.Before != nil {
				ch.Before = auditRedactedMarker
			}
			if ch.After != nil {
				ch.After = auditRedactedMarker
			}
			ch.Redacted = true
		}
		out = append(out, ch)
	}
	return out
}

func auditFlatten(prefix string, v map[string]interface{}, out map[string]interface{}) {
	for k, val := range v {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if sub, ok := val.(map[string]interface{}); ok && len(sub) > 0 {
			auditFlatten(path, sub, out)
			continue
		}
		out[path] = val
	}
}

func auditSame(a, b interface{}) bool {
	ja, ea := json.Marshal(a)
	jb, eb := json.Marshal(b)
	if ea != nil || eb != nil {
		return false
	}
	return string(ja) == string(jb)
}

// auditRouteFields anchor the separation between the model NAME (which may
// contain a dot, e.g. "gpt-4.1") and the route field.
// Order matters: "api_key_secret" before "api_key", otherwise the shorter prefix
// matches first and the model name comes out truncated. A field missing from this
// list makes the model name come out wrong AND splits the model into two events —
// exactly what happened with api_key on the first end-to-end verification.
var auditRouteFields = []string{
	"provider_model_id", "provider", "base_url", "api_key_secret", "api_key",
	"capabilities", "fallback", "region", "role_arn", "external_id",
	"kind", "enabled", "prefix_cache", "headers", "timeout_ms",
}

// auditDeriveModels translates changes under "routing.*" into the model-specific
// actions. It exists because there is no "add model" route: the Console writes
// models inside the routing key via PUT /admin/config, and recording that only as
// a generic config_update would be useless in exactly the case that motivated
// auditing ("who inserted this model?").
func auditDeriveModels(changes []auditChange) map[string][]auditChange {
	byModel := map[string][]auditChange{}
	for _, c := range changes {
		if !strings.HasPrefix(c.Path, "routing.") {
			continue
		}
		rest := strings.TrimPrefix(c.Path, "routing.")
		if rest == "" {
			continue
		}
		model := rest
		for _, f := range auditRouteFields {
			if i := strings.Index(rest, "."+f); i > 0 {
				model = rest[:i]
				break
			}
		}
		byModel[model] = append(byModel[model], c)
	}
	return byModel
}

// auditDeriveBundles translates changes under "bundles.*" into per-bundle events.
//
// The bundle name is the first segment after the prefix. Unlike routing, there is
// no anchoring by field list here: the name is the immediate segment and the rest
// of the path (layers, swap) is internal structure. A bundle name containing a
// dot is possible but unlikely, and the cost of getting it wrong is an event with
// a truncated target — not a lost event.
func auditDeriveBundles(changes []auditChange) map[string][]auditChange {
	byBundle := map[string][]auditChange{}
	for _, c := range changes {
		if !strings.HasPrefix(c.Path, "bundles.") {
			continue
		}
		rest := strings.TrimPrefix(c.Path, "bundles.")
		if rest == "" {
			continue
		}
		name := rest
		for _, f := range auditBundleFields {
			if i := strings.Index(rest, "."+f); i > 0 {
				name = rest[:i]
				break
			}
		}
		byBundle[name] = append(byBundle[name], c)
	}
	return byBundle
}

// auditBundleFields anchor the bundle name against the internal structure, for
// the same reason as auditRouteFields.
var auditBundleFields = []string{"layers", "swap", "name"}

func auditClassifyBundle(chs []auditChange) string {
	switch auditClassifyModel(chs) {
	case audModelAdd:
		return audBundleAdd
	case audModelRemove:
		return audBundleRemove
	default:
		return audBundleUpdate
	}
}

// auditDeriveSwapPolicy isolates `feature_policy.<f>.swap` changes per feature.
func auditDeriveSwapPolicy(changes []auditChange) map[string][]auditChange {
	byFeature := map[string][]auditChange{}
	for _, c := range changes {
		if !strings.HasPrefix(c.Path, "feature_policy.") || !strings.HasSuffix(c.Path, ".swap") {
			continue
		}
		feature := strings.TrimSuffix(strings.TrimPrefix(c.Path, "feature_policy."), ".swap")
		if feature == "" {
			continue
		}
		byFeature[feature] = append(byFeature[feature], c)
	}
	return byFeature
}

// auditDerivedElsewhere tells whether a path was already covered by a specific
// event and therefore must NOT ALSO appear in the generic config_update. Without
// this the same change would show up twice on screen and the customer would count
// two acts where there was one.
func auditDerivedElsewhere(path string) bool {
	switch {
	case strings.HasPrefix(path, "routing."):
		return true
	case strings.HasPrefix(path, "bundles."):
		return true
	case strings.HasPrefix(path, "feature_policy.") && strings.HasSuffix(path, ".swap"):
		return true
	}
	return false
}

func auditClassifyModel(chs []auditChange) string {
	allCreated, allRemoved := true, true
	for _, c := range chs {
		if c.Before != nil {
			allCreated = false
		}
		if c.After != nil {
			allRemoved = false
		}
	}
	switch {
	case allCreated && !allRemoved:
		return audModelAdd
	case allRemoved && !allCreated:
		return audModelRemove
	default:
		return audModelUpdate
	}
}

// auditCtx carries what comes from the REQUEST (actor and origin). Built once per
// request and passed along, so no emission call needs to reach the request
// object — which would open the door to deriving the actor from input.
type auditCtx struct {
	actor     auditActor
	sourceIP  string
	userAgent string
}

// curAudit holds the audit context of the request in flight.
//
// A package-level variable is safe here because of a Lambda property: one
// instance serves ONE request at a time (there is no concurrency inside the
// process). This avoids changing writeAudit's signature at ~15 call sites just to
// carry the IP and user agent. If this binary ever becomes a concurrent HTTP
// server (as the Core already can be), this must go back to being a parameter.
var curAudit auditCtx

// auditLegacyNames translates the OLD vocabulary (dotted notation, used by
// writeAudit since before this feature) into the canonical catalog. It exists so
// the new trail is not born with two names for the same action.
var auditLegacyNames = map[string]string{
	"team.create":  audTeamCreate,
	"team.rename":  audTeamRename,
	"team.archive": audTeamArchive,
	"team.remove":  audTeamRemove,
	"app.create":   audAppCreate,
	"app.rename":   audAppRename,
	"app.archive":  audAppArchive,
	"app.remove":   audAppRemove,
}

// auditCanonicalAction normalizes the action name to the catalog.
func auditCanonicalAction(action string) string {
	if c, ok := auditLegacyNames[action]; ok {
		return c
	}
	return action
}

// emitAudit publishes the event and NEVER propagates an error.
//
// The order (log → publish) is the guarantee that no event disappears silently:
// if PutEvents fails, the structured line is already in this domain's log group
// and the trail is reconstructible.
func emitAudit(ctx context.Context, ac auditCtx, action, org, scope, target string, changes []auditChange) {
	emitAuditDetail(ctx, ac, action, org, scope, target, "", changes)
}

// emitAuditDetail is the full form, with the human-readable text for actions that
// have no diff (inviting a member, archiving a team).
func emitAuditDetail(ctx context.Context, ac auditCtx, action, org, scope, target, detail string, changes []auditChange) {
	if org == "" || action == "" {
		return
	}
	action = auditCanonicalAction(action)
	truncated := false
	total := len(changes)
	if total > auditMaxChanges {
		changes, truncated = changes[:auditMaxChanges], true
	}
	ev := auditEvent{
		ContractVersion: auditContractVersion,
		EventID:         auditEventID(),
		Org:             org,
		Actor:           ac.actor,
		Action:          action,
		Category:        auditCategory[action],
		Scope:           scope,
		Target:          target,
		Detail:          detail,
		Changes:         changes,
		ChangeCount:     total,
		Truncated:       truncated,
		SourceIP:        ac.sourceIP,
		UserAgent:       ac.userAgent,
		TS:              time.Now().UTC().Format(time.RFC3339),
	}

	// 1) Raw trail in this domain's own log group, BEFORE publishing.
	if b, err := json.Marshal(map[string]interface{}{
		"event": "audit", "action": ev.Action, "org": ev.Org, "actor": ev.Actor.Email,
		"role": ev.Actor.Role, "scope": ev.Scope, "target": ev.Target,
		"change_count": ev.ChangeCount, "event_id": ev.EventID,
	}); err == nil {
		log.Println(string(b))
	}

	// 2) Best-effort publication.
	if auditBus == "" {
		return
	}
	if err := auditPutEvents(ctx, ev); err != nil {
		log.Printf(`{"event":"audit_emit_failed","action":%q,"org":%q,"error":%q}`,
			ev.Action, ev.Org, err.Error())
	}
}

// auditEventID generates the event identifier. It is not a library ULID (avoids a
// new dependency): a nanosecond timestamp + a monotonic counter is enough for the
// purpose, which is breaking ties between two events at the SAME instant —
// without it the writer's conditional write would discard the second as a
// duplicate.
var auditSeq int64

func auditEventID() string {
	auditSeq++
	return strconv.FormatInt(time.Now().UTC().UnixNano(), 36) + "-" + strconv.FormatInt(auditSeq, 36)
}

// auditPutEvents publishes to EventBridge via SigV4-signed HTTP, reusing the same
// mechanism already adopted in this domain for Cognito and for postconfirm's
// OrgCreated: sign the call instead of adding one more SDK (fewer dependencies,
// smaller binary).
func auditPutEvents(ctx context.Context, ev auditEvent) error {
	detail, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]interface{}{
		"Entries": []map[string]string{{
			"EventBusName": auditBus,
			"Source":       "aiplat.governance",
			"DetailType":   "aiplat.audit",
			"Detail":       string(detail),
		}},
	})
	if err != nil {
		return err
	}

	host := "events." + baseCfg.Region + ".amazonaws.com"
	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+host+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/x-amz-json-1.1")
	req.Header.Set("x-amz-target", "AWSEvents.PutEvents")

	creds, err := baseCfg.Credentials.Retrieve(ctx)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), "events", baseCfg.Region, time.Now()); err != nil {
		return err
	}
	res, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return &auditHTTPError{Status: res.StatusCode}
	}
	return nil
}

type auditHTTPError struct{ Status int }

func (e *auditHTTPError) Error() string { return "http " + strconv.Itoa(e.Status) }
