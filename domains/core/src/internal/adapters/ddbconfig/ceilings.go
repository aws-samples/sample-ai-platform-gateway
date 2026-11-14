// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package ddbconfig

import "strings"

// Narrow-only ceiling fold (CORE-SEC-001).
//
// deepMerge replaces scalars, so a child scope (TEAM#/APP#) could WIDEN every policy a
// parent set: turn guardrails off, raise the budget, lift rate limits, loosen the cache
// scope, clear a suspension, or repoint a model's route. Only allowed_models was a real
// ceiling (foldAllowed). This fold restores the README's contract — "a team can restrict
// what the org allows, never widen it" — for the rest of the ceiling keys, mirroring
// foldAllowed so the shared fixture at testdata/contracts/config-scope/scope-chain.json
// can drive it with no DynamoDB.
//
// The direction of "narrow" per key:
//   - guardrail flags (block_secrets, block_injection, mask_pii, no_store): parent OR
//     child — a child can turn a guardrail ON, never OFF.
//   - budget.limit_usd, rate_limits.*, max_thinking_tokens, request_timeout_ms: the
//     smaller POSITIVE value wins; a child 0/absent inherits the parent (0 means "unset",
//     not "raise to unlimited").
//   - budget.action: the stricter of block > degrade > alert.
//   - cache_scope: the stricter of app > team > deployment (a child may not read a wider
//     pool than its parent allows).
//   - status: "suspended" is sticky — a child cannot un-suspend.
//   - routing / pricing: ORG-only. A TEAM#/APP# scope may not override them at all
//     (base_url / role_arn / provider_model_id live inside routing), so they are dropped
//     from a sub-org level before the merge.

// ceilingSnapshot captures the parent (pre-merge) values the fold clamps against.
type ceilingSnapshot struct {
	budgetLimit    float64
	budgetLimitSet bool
	budgetAction   string
	rpm            float64
	rpmSet         bool
	tpm            float64
	tpmSet         bool
	maxThinking    float64
	maxThinkingSet bool
	reqTimeout     float64
	reqTimeoutSet  bool
	cacheScope     string
	guards         map[string]bool
	suspended      bool
}

// guardKeys are the guardrail flags folded by OR.
var guardKeys = []string{"block_secrets", "block_injection", "mask_pii", "no_store"}

// budgetActionRank / cacheScopeRank give the strictness order (higher = stricter).
var budgetActionRank = map[string]int{"alert": 0, "degrade": 1, "block": 2}
var cacheScopeRank = map[string]int{"deployment": 0, "team": 1, "app": 2}

func numAt(m map[string]interface{}, path ...string) (float64, bool) {
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

func strAt(m map[string]interface{}, path ...string) string {
	cur := m
	for i, p := range path {
		v, ok := cur[p]
		if !ok {
			return ""
		}
		if i == len(path)-1 {
			s, _ := v.(string)
			return s
		}
		cur, ok = v.(map[string]interface{})
		if !ok {
			return ""
		}
	}
	return ""
}

func boolAt(m map[string]interface{}, path ...string) bool {
	cur := m
	for i, p := range path {
		v, ok := cur[p]
		if !ok {
			return false
		}
		if i == len(path)-1 {
			b, _ := v.(bool)
			return b
		}
		cur, ok = v.(map[string]interface{})
		if !ok {
			return false
		}
	}
	return false
}

// snapshotCeilings reads the ceiling-relevant values out of the running (parent) config.
func snapshotCeilings(base map[string]interface{}) ceilingSnapshot {
	s := ceilingSnapshot{guards: map[string]bool{}}
	// CORE-SEC-009: the parent's effective budget ceiling is its Budget.Limit() — the
	// positive limit_usd, else monthly_usd (the old plan-seed key). Snapshotting only
	// limit_usd let a child escape by declaring limit_usd while the parent capped via
	// monthly_usd, because Budget.Limit() prefers the child's limit_usd and the fold never
	// saw the parent's monthly_usd ceiling.
	if lim, ok := numAt(base, "budget", "limit_usd"); ok && lim > 0 {
		s.budgetLimit, s.budgetLimitSet = lim, true
	} else if mon, ok := numAt(base, "budget", "monthly_usd"); ok && mon > 0 {
		s.budgetLimit, s.budgetLimitSet = mon, true
	}
	s.budgetAction = strAt(base, "budget", "action")
	s.rpm, s.rpmSet = numAt(base, "rate_limits", "requests_per_minute")
	s.tpm, s.tpmSet = numAt(base, "rate_limits", "tokens_per_minute")
	s.maxThinking, s.maxThinkingSet = numAt(base, "max_thinking_tokens")
	s.reqTimeout, s.reqTimeoutSet = numAt(base, "request_timeout_ms")
	s.cacheScope = strAt(base, "cache_scope")
	for _, k := range guardKeys {
		s.guards[k] = boolAt(base, "guardrails", k)
	}
	s.suspended = strAt(base, "status") == "suspended"
	return s
}

// narrowerPositive returns the smaller positive value; a non-positive child inherits the
// parent, and a non-positive parent lets the child's positive value stand.
func narrowerPositive(parent float64, parentSet bool, child float64) float64 {
	if child <= 0 {
		return parent
	}
	if !parentSet || parent <= 0 {
		return child
	}
	if parent < child {
		return parent
	}
	return child
}

// stripOrgOnly removes the ORG-only keys from a sub-org (TEAM#/APP#) config level, so a
// child cannot override routing/pricing (nor base_url/role_arn, which live inside
// routing). Called on the child map BEFORE the merge.
func stripOrgOnly(level map[string]interface{}, belowOrg bool) {
	if !belowOrg {
		return
	}
	delete(level, "routing")
	delete(level, "pricing")
}

// foldCeilings clamps the merged config (which deepMerge has already overlaid with the
// child level) back to be no looser than the parent snapshot. It mutates base in place.
// below is true when the child level being folded is a TEAM#/APP# scope; it gates the
// rules that must NOT apply to an ORG (or global) write — see cache_scope (CORE-FUN-007).
func foldCeilings(base map[string]interface{}, p ceilingSnapshot, below bool) {
	// budget — smaller positive effective limit wins. CORE-SEC-009: compare the CHILD's
	// effective limit (limit_usd if positive, else monthly_usd — the same precedence as
	// Budget.Limit()) so a child cannot widen via whichever budget key the parent did not
	// use. The clamped value is written to limit_usd, which Limit() prefers.
	if bm, ok := base["budget"].(map[string]interface{}); ok {
		childLimit, childSet := 0.0, false
		if v, ok := bm["limit_usd"].(float64); ok && v > 0 {
			childLimit, childSet = v, true
		} else if v, ok := bm["monthly_usd"].(float64); ok && v > 0 {
			childLimit, childSet = v, true
		}
		if childSet {
			bm["limit_usd"] = narrowerPositive(p.budgetLimit, p.budgetLimitSet, childLimit)
		} else if p.budgetLimitSet {
			bm["limit_usd"] = p.budgetLimit
		}
		// budget.action — the stricter wins.
		if p.budgetAction != "" {
			child, _ := bm["action"].(string)
			if budgetActionRank[p.budgetAction] > budgetActionRank[child] {
				bm["action"] = p.budgetAction
			}
		}
	}
	// rate_limits — smaller positive wins per field.
	if rm, ok := base["rate_limits"].(map[string]interface{}); ok {
		if child, ok := rm["requests_per_minute"].(float64); ok {
			rm["requests_per_minute"] = narrowerPositive(p.rpm, p.rpmSet, child)
		} else if p.rpmSet {
			rm["requests_per_minute"] = p.rpm
		}
		if child, ok := rm["tokens_per_minute"].(float64); ok {
			rm["tokens_per_minute"] = narrowerPositive(p.tpm, p.tpmSet, child)
		} else if p.tpmSet {
			rm["tokens_per_minute"] = p.tpm
		}
	}
	// max_thinking_tokens / request_timeout_ms — ceilings, smaller positive wins.
	if child, ok := base["max_thinking_tokens"].(float64); ok {
		base["max_thinking_tokens"] = narrowerPositive(p.maxThinking, p.maxThinkingSet, child)
	} else if p.maxThinkingSet {
		base["max_thinking_tokens"] = p.maxThinking
	}
	if child, ok := base["request_timeout_ms"].(float64); ok {
		base["request_timeout_ms"] = narrowerPositive(p.reqTimeout, p.reqTimeoutSet, child)
	} else if p.reqTimeoutSet {
		base["request_timeout_ms"] = p.reqTimeout
	}
	// guardrail flags — parent OR child (a child can only ADD blocking).
	if gm, ok := base["guardrails"].(map[string]interface{}); ok {
		for _, k := range guardKeys {
			if p.guards[k] {
				gm[k] = true
			}
		}
	} else if anyGuard(p.guards) {
		gm := map[string]interface{}{}
		for _, k := range guardKeys {
			if p.guards[k] {
				gm[k] = true
			}
		}
		base["guardrails"] = gm
	}
	// cache_scope — the stricter wins. CORE-SEC-009: an UNDECLARED parent scope is treated
	// as the documented default "team". Without this, a child could set cache_scope to the
	// looser "deployment" whenever no ancestor declared a scope — letting a TEAM# item read
	// answers cached by other teams. The default only bounds a child that DECLARES a scope;
	// if the child declares none, behaviour is unchanged (inherit the parent's explicit
	// scope, if any).
	// CORE-FUN-007: the "undeclared parent defaults to team" rule applies ONLY when the
	// child is a TEAM#/APP# scope (below). It exists so a TEAM/APP cannot silently opt into
	// the looser "deployment" scope and read other teams' cached answers. An ORG (or global)
	// write has no such floor: if no ancestor declared a scope, an ORG may legitimately
	// choose "deployment" (deployment-wide caching is a per-org decision). Defaulting at
	// every fold step made "deployment" unreachable everywhere.
	parentScope := p.cacheScope
	if parentScope == "" && below {
		parentScope = "team"
	}
	if child, ok := base["cache_scope"].(string); ok && child != "" {
		if parentScope != "" && cacheScopeRank[parentScope] > cacheScopeRank[child] {
			base["cache_scope"] = parentScope
		}
	} else if p.cacheScope != "" {
		if _, has := base["cache_scope"]; !has {
			base["cache_scope"] = p.cacheScope
		}
	}
	// status — suspension is sticky.
	if p.suspended {
		base["status"] = "suspended"
	}
}

func anyGuard(g map[string]bool) bool {
	for _, k := range guardKeys {
		if g[k] {
			return true
		}
	}
	return false
}

// belowOrg reports whether a scope key is below the ORG level (a TEAM# or APP# scope),
// where routing/pricing may not be overridden.
func belowOrg(scopeKey string) bool {
	return strings.Contains(scopeKey, "#TEAM#") || strings.Contains(scopeKey, "#APP#")
}
