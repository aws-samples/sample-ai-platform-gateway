// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package govcore

// Narrow-only ceiling detection (GOV-SEC-001).
//
// The WRITE-side mirror of core's foldCeilings: given the effective PARENT config and a
// child scope (TEAM#/APP#) being written, CeilingViolations returns the policy keys the
// child WIDENS. config-api rejects a non-empty result with 400 scope_widening, so a child
// can never loosen a guardrail, raise a budget/rate ceiling, loosen the cache scope,
// clear a suspension, or repoint a model's route — the same contract the README states
// and the same cases the shared fixture (testdata/contracts/config-scope) drives for both
// domains. Duplicated on purpose (D3): no shared library across domains.
//
// Directions match core: guardrail flags may only go ON (parent true + child false is a
// violation); budget.limit_usd / rate_limits.* / max_thinking_tokens / request_timeout_ms
// may only shrink (a positive child above a positive parent is a violation); budget.action
// and cache_scope may only grow stricter; status cannot leave "suspended"; routing/pricing
// are ORG-only (any sub-org override is a violation).

var govGuardKeys = []string{"block_secrets", "block_injection", "mask_pii", "no_store"}
var govBudgetActionRank = map[string]int{"alert": 0, "degrade": 1, "block": 2}
var govCacheScopeRank = map[string]int{"deployment": 0, "team": 1, "app": 2}

func gNum(m map[string]interface{}, path ...string) (float64, bool) {
	cur := m
	for i, p := range path {
		v, ok := cur[p]
		if !ok {
			return 0, false
		}
		if i == len(path)-1 {
			f, ok := v.(float64)
			return f, ok
		}
		cur, ok = v.(map[string]interface{})
		if !ok {
			return 0, false
		}
	}
	return 0, false
}

func gStr(m map[string]interface{}, path ...string) (string, bool) {
	cur := m
	for i, p := range path {
		v, ok := cur[p]
		if !ok {
			return "", false
		}
		if i == len(path)-1 {
			s, ok := v.(string)
			return s, ok
		}
		cur, ok = v.(map[string]interface{})
		if !ok {
			return "", false
		}
	}
	return "", false
}

func gBool(m map[string]interface{}, path ...string) (bool, bool) {
	cur := m
	for i, p := range path {
		v, ok := cur[p]
		if !ok {
			return false, false
		}
		if i == len(path)-1 {
			b, ok := v.(bool)
			return b, ok
		}
		cur, ok = v.(map[string]interface{})
		if !ok {
			return false, false
		}
	}
	return false, false
}

// govEffectiveBudget is a config's effective budget ceiling: the positive limit_usd, else
// monthly_usd (the old plan-seed key) — the same precedence as the Core's Budget.Limit().
// CORE-SEC-009: checking only limit_usd let a child widen by declaring limit_usd while the
// parent capped via monthly_usd.
func govEffectiveBudget(m map[string]interface{}) (float64, bool) {
	if v, ok := gNum(m, "budget", "limit_usd"); ok && v > 0 {
		return v, true
	}
	if v, ok := gNum(m, "budget", "monthly_usd"); ok && v > 0 {
		return v, true
	}
	return 0, false
}

// widensNum reports a positive child value above a positive parent ceiling. A child
// value of 0/absent inherits the parent and is never a violation.
func widensNum(parent map[string]interface{}, child map[string]interface{}, path ...string) bool {
	cv, cok := gNum(child, path...)
	if !cok || cv <= 0 {
		return false
	}
	pv, pok := gNum(parent, path...)
	return pok && pv > 0 && cv > pv
}

// CeilingViolations returns the keys a child scope widens against the effective parent.
// belowOrg marks a TEAM#/APP# write, where routing/pricing may not be overridden at all.
func CeilingViolations(parent, child map[string]interface{}, belowOrg bool) []string {
	var v []string

	// CORE-SEC-009: compare EFFECTIVE budget limits (limit_usd else monthly_usd) so a
	// child cannot widen via whichever budget key the parent did not use.
	if cv, cok := govEffectiveBudget(child); cok {
		if pv, pok := govEffectiveBudget(parent); pok && cv > pv {
			v = append(v, "budget.limit_usd")
		}
	}
	if ca, ok := gStr(child, "budget", "action"); ok {
		if pa, ok := gStr(parent, "budget", "action"); ok && govBudgetActionRank[pa] > govBudgetActionRank[ca] {
			v = append(v, "budget.action")
		}
	}
	if widensNum(parent, child, "rate_limits", "requests_per_minute") {
		v = append(v, "rate_limits.requests_per_minute")
	}
	if widensNum(parent, child, "rate_limits", "tokens_per_minute") {
		v = append(v, "rate_limits.tokens_per_minute")
	}
	if widensNum(parent, child, "max_thinking_tokens") {
		v = append(v, "max_thinking_tokens")
	}
	if widensNum(parent, child, "request_timeout_ms") {
		v = append(v, "request_timeout_ms")
	}
	for _, k := range govGuardKeys {
		cb, cok := gBool(child, "guardrails", k)
		if !cok || cb {
			continue // child did not set it, or set it ON (narrowing)
		}
		if pb, ok := gBool(parent, "guardrails", k); ok && pb {
			v = append(v, "guardrails."+k)
		}
	}
	if cs, ok := gStr(child, "cache_scope"); ok {
		// CORE-SEC-009 / CORE-FUN-007: an UNDECLARED parent scope defaults to "team" ONLY
		// when the child is a TEAM#/APP# scope (belowOrg) — that is the floor that stops a
		// team opting into the looser "deployment" scope. An ORG write against a silent
		// global has no such floor: the org may declare "deployment". Defaulting for ORG
		// writes too made a legitimate ORG "deployment" write a false scope_widening.
		ps, pok := gStr(parent, "cache_scope")
		if !pok && belowOrg {
			ps = "team"
		}
		if govCacheScopeRank[ps] > govCacheScopeRank[cs] {
			v = append(v, "cache_scope")
		}
	}
	if st, ok := gStr(child, "status"); ok && st != "suspended" {
		if ps, ok := gStr(parent, "status"); ok && ps == "suspended" {
			v = append(v, "status")
		}
	}
	if belowOrg {
		if _, ok := child["routing"]; ok {
			v = append(v, "routing")
		}
		if _, ok := child["pricing"]; ok {
			v = append(v, "pricing")
		}
	}
	return v
}
