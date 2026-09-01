// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package auditcore

import "strings"

// Actor type. The distinction exists for a reason of trust, not taxonomy: if AIPlat
// touched a customer's account, the customer has the right to see that in THEIR trail —
// so the type needs to travel in the record and show up on screen.
const (
	ActorCustomer = "customer"
	ActorOperator = "platform_operator"
)

// Roles (they mirror Cognito's custom:role claim).
const (
	RoleOwner         = "owner"
	RoleAdmin         = "admin"
	RoleBilling       = "billing"
	RoleDev           = "dev"
	RolePlatformAdmin = "platform_admin"
)

// Actor is whoever performed the action. ALWAYS derived from the JWT claims — never
// from the body, the query string or a client header. It is that rule that gives the
// trail evidentiary value: authorship derived from input is forgeable authorship.
type Actor struct {
	Email string `json:"email"`
	Sub   string `json:"sub,omitempty"`
	Role  string `json:"role"`
	Type  string `json:"type"`
}

// Event is the audit event in the versioned contract exchanged between domains.
type Event struct {
	ContractVersion string `json:"contract_version"`
	EventID         string `json:"event_id"`
	Org             string `json:"org"`
	Actor           Actor  `json:"actor"`
	Action          string `json:"action"`
	Category        string `json:"category"`
	Scope           string `json:"scope,omitempty"`
	Target          string `json:"target,omitempty"`
	// Detail is short, readable text for the action that has NO diff (inviting a
	// member, archiving a team). It never carries a secret — it is human context
	// ("role=admin team=platform"), not a field value.
	Detail      string   `json:"detail,omitempty"`
	Changes     []Change `json:"changes,omitempty"`
	ChangeCount int      `json:"change_count"`
	Truncated   bool     `json:"truncated,omitempty"`
	SourceIP    string   `json:"source_ip,omitempty"`
	UserAgent   string   `json:"user_agent,omitempty"`
	TS          string   `json:"ts"`
}

// ContractVersion of the event. It exists so the writer can evolve without breaking an
// older emitter during a partial deploy — the domains do not ship together.
const ContractVersion = "1"

// ActorTypeFor classifies the actor by role. platform_admin is a platform operator,
// never a role a customer can assign.
func ActorTypeFor(role string) string {
	if role == RolePlatformAdmin {
		return ActorOperator
	}
	return ActorCustomer
}

// NewActor builds the actor, normalizing the e-mail to lowercase (the same
// normalization govcore.NewAuditEntry already did) and deriving the type from the role.
// PURE.
func NewActor(email, sub, role string) Actor {
	return Actor{
		Email: strings.ToLower(strings.TrimSpace(email)),
		Sub:   sub,
		Role:  role,
		Type:  ActorTypeFor(role),
	}
}

// RetentionDays is the trail's retention window, in days, for this deployment.
//
// Single-client deployment: one fixed window, not a tier lookup. 365 days errs on
// the side of keeping too much — lost audit data is unrecoverable, and keeping
// small items for longer costs almost nothing.
const RetentionDays = 365

// Refusal reason. A code (not a message) because the Console renders it, and a
// code is what lets the string translate without becoming a lookup key itself.
const DenyRole = "audit_role_forbidden"

// CanRead decides whether the requester may read the trail: owner/admin (and the
// platform operator) only. A dev does not need to know who changed the routing
// policy; billing has no reason to see a member removed. Whoever cannot change
// the policy also does not need to audit who changed it.
func CanRead(role string, isOperator bool) (bool, string) {
	if isOperator || role == RolePlatformAdmin {
		return true, ""
	}
	if role != RoleOwner && role != RoleAdmin {
		return false, DenyRole
	}
	return true, ""
}

// SortKeys builds the three sort keys in a single place, so the format does not diverge
// between the writer (which stores) and the API (which queries by prefix).
//
// The eventID in the suffix is not decoration: without it, two legitimate events at the
// SAME instant would collide on the key, and the conditional write (which exists for
// idempotency) would reject the second as a duplicate — the trail would lose a genuine
// record precisely under concurrency.
func SortKeys(ts, eventID, category, actorEmail string) (sk, catSK, actorSK string) {
	suffix := ts + "#" + eventID
	sk = "TS#" + suffix
	catSK = "CAT#" + category + "#TS#" + suffix
	actorSK = "ACTOR#" + strings.ToLower(strings.TrimSpace(actorEmail)) + "#TS#" + suffix
	return
}

// CategoryPrefix and ActorPrefix build the query prefix for the LSIs, so the API does
// not reassemble the string by hand and diverge from the SortKeys format.
func CategoryPrefix(category string) string { return "CAT#" + category + "#TS#" }
func ActorPrefix(email string) string {
	return "ACTOR#" + strings.ToLower(strings.TrimSpace(email)) + "#TS#"
}

// PartitionKey is the partition key of an org's trail. The org is IN the key: a Query
// never crosses orgs, not even through a filter bug. Structural isolation, not
// isolation by a correct WHERE.
func PartitionKey(org string) string { return "AUDIT#" + org }
