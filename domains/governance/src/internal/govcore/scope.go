// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package govcore is the PURE domain of Governance: the config scope chain, plan
// rules, the role matrix (RBAC) and credit validation.
//
// Boundary rule (hexagonal-refactor, R5/D5): nothing here may reach an SDK, the
// network, a file, the environment, the clock or randomness. Every time
// dependency comes in as a parameter. boundary_test.go verifies this through the
// transitive closure.
//
// The scope chain is DUPLICATED on purpose between Core and Governance (D3): it is
// a contract implemented twice, not a shared library. The drift risk is covered by
// the contract test over the common fixture (scope_contract_test.go).
package govcore

// ScopeKey builds the key of the MOST specific scope provided.
// Progressive hierarchy: absent levels collapse.
func ScopeKey(org, team, app string) string {
	if org == "" {
		return "global"
	}
	k := "ORG#" + org
	if team == "" && app == "" {
		return k
	}
	if team == "" {
		team = "default"
	}
	k += "#TEAM#" + team
	if app != "" {
		k += "#APP#" + app
	}
	return k
}

// ScopeKeys returns the effective INHERITANCE CHAIN, from least to most specific.
//
// Aligned with the Core (core/internal/adapters/ddbconfig.scopeKeys): when the
// team is not provided it resolves to `default` and the chain ALWAYS includes
// ORG#…#TEAM#default. That is what the gateway actually applies (team always
// resolves to default), so the effective config the console shows now matches
// the one the Core executes. Verified against the same fixture by both domains
// (scope_contract_test.go), with no shared code.
//
// Note: the chain (plural) is for the effective MERGE; the scope WRITE target is
// ScopeKey (singular), which for an org without a team returns ORG# — writing
// without a team stores at the org level, not under TEAM#default.
func ScopeKeys(org, team, app string) []string {
	keys := []string{"global"}
	if org == "" {
		return keys
	}
	keys = append(keys, "ORG#"+org)
	if team == "" {
		team = "default"
	}
	keys = append(keys, "ORG#"+org+"#TEAM#"+team)
	if app != "" {
		keys = append(keys, "ORG#"+org+"#TEAM#"+team+"#APP#"+app)
	}
	return keys
}

// DeepMerge overlays src onto dst: maps merge by key; scalars and lists replace.
// It is what allows an org to add a model without repeating the whole catalog.
func DeepMerge(dst, src map[string]interface{}) {
	for k, v := range src {
		if sv, ok := v.(map[string]interface{}); ok {
			if dv, ok2 := dst[k].(map[string]interface{}); ok2 {
				DeepMerge(dv, sv)
				continue
			}
			cp := map[string]interface{}{}
			DeepMerge(cp, sv)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}
