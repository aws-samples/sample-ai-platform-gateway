// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package govcore

// Role matrix (RBAC) — PURE, receiving the claims as input DATA.
// This is where a mistake becomes privilege escalation, which makes it the most
// valuable point in the domain: the property in rbac_test.go demands a TOTAL and
// conservative verdict.

// Claims is the authorization data arriving from the JWT (validated by API Gateway).
type Claims struct {
	Org   string
	Role  string
	Team  string
	Email string
}

// Access is the verdict derived from the claims.
type Access struct {
	// IsPlatform: platform operator (platform_admin). May cross orgs and write to
	// the global scope.
	IsPlatform bool
	// CanAdmin: may write policy/config and credentials (owner/admin).
	CanAdmin bool
	// TeamScoped: locked to a team — only operates within that team's scope.
	TeamScoped bool
}

// Resolve derives the access verdict from the claims.
//
// Conservative by construction: an unknown (or absent) role NEVER gets CanAdmin
// nor IsPlatform. Owner/admin/platform_admin see the whole org (never
// team-scoped); billing/dev with a `team` claim stay locked to that team.
func Resolve(c Claims) Access {
	isPlatform := c.Role == "platform_admin"
	return Access{
		IsPlatform: isPlatform,
		CanAdmin:   isPlatform || c.Role == "owner" || c.Role == "admin",
		TeamScoped: c.Team != "" && c.Role != "owner" && c.Role != "admin" && !isPlatform,
	}
}

// ForceOrg guarantees an org user only operates within its own scope.
// The "global" scope (it affects the whole platform, not just one team/app) is
// exclusive to platform_admin.
//
// Returns (effective org, ok). ok=false when a regular user has no org determined
// in the token — in that case we NEVER fall back to another org (the golden rule
// of isolation): the shell must answer 403.
func ForceOrg(a Access, c Claims, param string) (string, bool) {
	if a.IsPlatform {
		return param, true // may even write to global (empty param)
	}
	if c.Org == "" {
		return "", false
	}
	return c.Org, true
}

// EffTeam forces the claim's team for team-scoped users (ignoring the parameter);
// for everyone else it honors what came in.
func EffTeam(a Access, c Claims, param string) string {
	if a.TeamScoped {
		return c.Team
	}
	return param
}
