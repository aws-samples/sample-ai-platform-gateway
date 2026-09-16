// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ddbconfig is the outbound adapter for the Core's effective config, read
// from the Governance config table as a CONTRACT (never a synchronous Lambda call).
//
// (loadConfig, scopeKeys, deepMerge and the 15s cache) without rewriting the logic.
// It preserves the mandatory FALLBACK: unavailable config does not take the gateway
// down — the caller passes the environment defaults as `base`, and whatever the
// table brings is merged on top (the most specific wins). An empty table ⇒ returns
// only the defaults. Converting the effective map into the Config type (the
// handler's) stays with the caller: this package is infrastructure and does not
// know the decision type.
package ddbconfig

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aiplat/core/internal/ports"
)

const cacheTTL = 15 * time.Second

type entry struct {
	merged      map[string]interface{}
	budgetScope string
	limitsScope string
	at          time.Time
}

// Store is the production adapter over the config table, with a short cache.
type Store struct {
	ddb   *dynamodb.Client
	table string
	// org is this deployment's single org (see scopeKeys) — never empty in
	// production; New falls back to "default" so a missing env var degrades to
	// a stable (if wrong) scope instead of silently reading nothing forever.
	org string

	mu    sync.Mutex
	cache map[string]entry
}

var _ ports.ConfigStore = (*Store)(nil) // compile-time assertion

// New builds the adapter with the client, the table name (may be "") and the
// deployment's org (may be ""; falls back to "default").
func New(ddb *dynamodb.Client, table string, org string) *Store {
	if org == "" {
		org = "default"
	}
	return &Store{ddb: ddb, table: table, org: org, cache: map[string]entry{}}
}

// Reset clears the cache. Used in tests to isolate scenarios.
func (s *Store) Reset() {
	s.mu.Lock()
	s.cache = map[string]entry{}
	s.mu.Unlock()
}

// scopeKeys returns the config keys from the least to the most specific.
//
// Single-org model: the deployment has exactly one org (never chosen per
// request), but the config table is STILL partitioned by org — Governance
// writes every scope under ORG#<org>#... (see govcore.ScopeKeys) because it is
// the shared control-plane schema across domains. The Core must read the SAME
// keys it does not write, or every team/app-level setting configured through
// the console (model catalog, budget, rate limits, cache) silently never
// reaches the gateway. `org` here is the single deployment org (DEPLOYMENT_ORG
// env var / Contract of Environment), not a per-request parameter.
func scopeKeys(org, team, app string) []string {
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

// allowedModelsKey is the ONE config key resolved by intersection instead of
// replacement, so a parent scope is a real ceiling and not merely a default.
//
// Deliberately key-scoped rather than a change to deepMerge: model_order,
// feature_policy.models, bundle layers and routing all ride the same merge and
// DEPEND on replace (see the comment on Config.ModelOrder — "It is a LIST on
// purpose: lists replace on inheritance"). Making the merge generically
// intersecting would silently break reordering at the team level.
const allowedModelsKey = "allowed_models"

// declaredList reports a config value as a list plus whether the key was DECLARED.
//
// Present-but-empty counts as declared. That distinction carries the whole
// intersection design: `[]` means "deny everything", absent means "no restriction".
func declaredList(v interface{}) ([]interface{}, bool) {
	if v == nil {
		return nil, false
	}
	l, ok := v.([]interface{})
	if !ok {
		return nil, false
	}
	return l, true
}

// intersectAllowed resolves a parent ceiling against a child declaration.
//
//	parent not declared → the child's list stands (nothing above to narrow it);
//	child not declared  → the parent's ceiling is inherited unchanged;
//	both declared       → set intersection, in the CHILD's order.
//
// A genuinely empty intersection (disjoint declarations) comes back as a NON-NIL
// empty slice, which the gateway reads as "deny everything" — see
// gateway.Config.allowed. That is the load-bearing part: the predicate used to read
// len==0 as "allow everything", so intersecting two restrictions would have granted
// the entire catalog. Two denials producing full access is the worst possible
// direction for this rule to fail in.
//
// Always allocates. ddbconfig.Effective mutates `base` in place and then caches it
// BY REFERENCE for 15s, so filtering a slice in place would corrupt a value the
// caller still holds and the cache is about to serve.
func intersectAllowed(parent []interface{}, parentDeclared bool, child []interface{}, childDeclared bool) ([]interface{}, bool) {
	switch {
	case !parentDeclared && !childDeclared:
		return nil, false
	case !parentDeclared:
		out := make([]interface{}, len(child))
		copy(out, child)
		return out, true
	case !childDeclared:
		out := make([]interface{}, len(parent))
		copy(out, parent)
		return out, true
	}
	in := make(map[interface{}]bool, len(parent))
	for _, v := range parent {
		in[v] = true
	}
	out := make([]interface{}, 0, len(child))
	for _, v := range child {
		if in[v] {
			out = append(out, v)
		}
	}
	return out, true
}

// foldAllowed folds ONE config level onto the running ceiling. It is the exact code
// the merge walk uses, factored out so the shared cross-domain fixture at
// testdata/contracts/config-scope/scope-chain.json can drive it directly — the
// merge walk itself needs a DynamoDB client and is not reachable from a unit test.
func foldAllowed(acc []interface{}, accDeclared bool, level map[string]interface{}) ([]interface{}, bool) {
	child, childDeclared := declaredList(level[allowedModelsKey])
	return intersectAllowed(acc, accDeclared, child, childDeclared)
}

// deepMerge overlays src onto dst: maps are merged key by key, scalars/lists are
// replaced. That is what lets an org add a model without repeating the catalog.
// allowed_models is the single exception and is folded separately by Effective.
func deepMerge(dst, src map[string]interface{}) {
	for k, v := range src {
		if sv, ok := v.(map[string]interface{}); ok {
			if dv, ok2 := dst[k].(map[string]interface{}); ok2 {
				deepMerge(dv, sv)
				continue
			}
			cp := map[string]interface{}{}
			deepMerge(cp, sv)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

// Effective resolves the EFFECTIVE config for team/app: it starts from `base`
// (environment defaults, supplied by the caller) and merges global → TEAM → APP.
// It returns the merged map and the scopes that DEFINED budget and rate_limits
// (the enforcement counter uses that scope). 15s cache per team|app.
//
// Single-org model: org is the deployment's own (s.org), never a parameter — a
// request only ever chooses team/app.
// IMPORTANT: `base` is mutated (the merge writes into it). The caller must pass its
// own map per call — as the original loadConfig did.
func (s *Store) Effective(ctx context.Context, team, app string, base map[string]interface{}) (map[string]interface{}, string, string) {
	ck := team + "|" + app
	s.mu.Lock()
	if e, ok := s.cache[ck]; ok && time.Since(e.at) < cacheTTL {
		s.mu.Unlock()
		return e.merged, e.budgetScope, e.limitsScope
	}
	s.mu.Unlock()

	budgetScope, limitsScope := "", ""

	if s.table != "" {
		keys := scopeKeys(s.org, team, app)
		reqKeys := make([]map[string]ddbtypes.AttributeValue, 0, len(keys))
		for _, k := range keys {
			reqKeys = append(reqKeys, map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: k}})
		}
		out, err := s.ddb.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{
			RequestItems: map[string]ddbtypes.KeysAndAttributes{s.table: {Keys: reqKeys}},
		})
		if err == nil {
			// Index the result (Batch does not guarantee order) and merge in the right order.
			byPk := map[string]map[string]interface{}{}
			for _, it := range out.Responses[s.table] {
				pk, _ := it["pk"].(*ddbtypes.AttributeValueMemberS)
				cfgv, _ := it["config"].(*ddbtypes.AttributeValueMemberS)
				if pk == nil || cfgv == nil {
					continue
				}
				var m map[string]interface{}
				if json.Unmarshal([]byte(cfgv.Value), &m) == nil {
					byPk[pk.Value] = m
				}
			}
			for _, k := range keys {
				if m, ok := byPk[k]; ok {
					// allowed_models is captured BEFORE the merge and folded after,
					// because deepMerge would replace the parent's ceiling with the
					// child's list. Doing it here rather than inside deepMerge keeps
					// every other list on replace semantics.
					parentAllowed, parentDeclared := declaredList(base[allowedModelsKey])
					deepMerge(base, m)
					if folded, declared := foldAllowed(parentAllowed, parentDeclared, m); declared {
						base[allowedModelsKey] = folded
					} else {
						delete(base, allowedModelsKey)
					}
					if _, has := m["budget"]; has {
						budgetScope = k
					}
					if _, has := m["rate_limits"]; has {
						limitsScope = k
					}
				}
			}
		}
	}

	s.mu.Lock()
	s.cache[ck] = entry{merged: base, budgetScope: budgetScope, limitsScope: limitsScope, at: time.Now()}
	s.mu.Unlock()
	return base, budgetScope, limitsScope
}
