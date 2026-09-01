// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package govcore

import (
	"testing"
	"testing/quick"
)

// Truth table (case by case) of the role matrix.
func TestResolveTruthTable(t *testing.T) {
	cases := []struct {
		role, team                       string
		isPlatform, canAdmin, teamScoped bool
	}{
		{"platform_admin", "", true, true, false},
		{"platform_admin", "sre", true, true, false},
		{"owner", "", false, true, false},
		{"owner", "sre", false, true, false},
		{"admin", "", false, true, false},
		{"admin", "sre", false, true, false},
		{"billing", "", false, false, false},
		{"billing", "sre", false, false, true},
		{"dev", "", false, false, false},
		{"dev", "sre", false, false, true},
		{"", "", false, false, false},
		{"", "sre", false, false, true},
		{"nonsense", "", false, false, false},
		{"nonsense", "sre", false, false, true},
	}
	for _, c := range cases {
		a := Resolve(Claims{Org: "acme", Role: c.role, Team: c.team})
		if a.IsPlatform != c.isPlatform || a.CanAdmin != c.canAdmin || a.TeamScoped != c.teamScoped {
			t.Errorf("Resolve(role=%q,team=%q)=%+v, want {isPlatform:%v canAdmin:%v teamScoped:%v}",
				c.role, c.team, a, c.isPlatform, c.canAdmin, c.teamScoped)
		}
	}
}

// Property 10 (design): the role matrix is TOTAL and CONSERVATIVE.
//
// For ANY combination of claims:
//  1. the verdict is defined (Resolve never panics);
//  2. an unknown role NEVER gets administration nor platform;
//  3. an undetermined org NEVER reaches another org nor the global scope.
func TestResolveIsTotalAndConservative(t *testing.T) {
	knownAdmin := map[string]bool{"owner": true, "admin": true, "platform_admin": true}

	prop := func(org, role, team, param string) bool {
		c := Claims{Org: org, Role: role, Team: team}
		a := Resolve(c)

		// (2) Only known roles administer; only platform_admin is platform.
		if a.CanAdmin && !knownAdmin[role] {
			return false
		}
		if a.IsPlatform && role != "platform_admin" {
			return false
		}
		// owner/admin/platform are never team-scoped.
		if a.TeamScoped && knownAdmin[role] {
			return false
		}

		// (3) Isolation: a regular user (not platform) with no org in the token
		// NEVER reaches another org nor global.
		gotOrg, ok := ForceOrg(a, c, param)
		if !a.IsPlatform {
			if org == "" {
				if ok {
					return false // cannot authorize without an org
				}
			} else {
				// org determined → always locked to its own org, ignoring the param.
				if !ok || gotOrg != org {
					return false
				}
			}
		} else {
			// platform honors the parameter (including global via an empty param).
			if !ok || gotOrg != param {
				return false
			}
		}

		// EffTeam: team-scoped forces the claim's team; otherwise it honors the param.
		et := EffTeam(a, c, param)
		if a.TeamScoped {
			if et != team {
				return false
			}
		} else if et != param {
			return false
		}
		return true
	}

	if err := quick.Check(prop, &quick.Config{MaxCount: 5000}); err != nil {
		t.Error(err)
	}
}

// Explicit reinforcement: no random role string becomes admin.
func TestUnknownRoleNeverAdmin(t *testing.T) {
	prop := func(role string) bool {
		if role == "owner" || role == "admin" || role == "platform_admin" {
			return true // known roles: out of scope for this property
		}
		a := Resolve(Claims{Org: "acme", Role: role, Team: ""})
		return !a.CanAdmin && !a.IsPlatform
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 5000}); err != nil {
		t.Error(err)
	}
}
